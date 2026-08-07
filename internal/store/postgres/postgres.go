// Package postgres is the real store.Store backed by Postgres (single source of
// truth). The in-memory store and this one satisfy the same interface, so nothing else in
// the system changes. A tiny built-in migrator applies migrations/*.sql on startup.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

var _ store.Store = (*Store)(nil)

type Store struct {
	pool *pgxpool.Pool
}

// New connects, pings, and runs migrations from migrationsDir. It retries the initial
// connection with backoff so startup is order-independent under compose (the api can come
// up before Postgres is accepting connections).
func New(ctx context.Context, dsn, migrationsDir string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pingWithRetry(ctx, pool, 30*time.Second); err != nil {
		pool.Close()
		return nil, err
	}
	if err := migrate(ctx, pool, migrationsDir); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// pingWithRetry pings until it succeeds or maxWait elapses, so a not-yet-ready database
// (common when containers start together) doesn't crash the process.
func pingWithRetry(ctx context.Context, pool *pgxpool.Pool, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	var err error
	for {
		if err = pool.Ping(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres: not reachable after %s: %w", maxWait, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (s *Store) Close() { s.pool.Close() }

// Pool exposes the connection pool so River can share the same database.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// WithLock runs fn while holding the named Postgres advisory lock - a mutex shared by every api
// and worker replica on this database (see store.Store.WithLock and lock.go).
func (s *Store) WithLock(name string, fn func() error) error {
	return WithAdvisoryLock(context.Background(), s.pool, name, fn)
}

const clusterCols = `id, name, size, network_cidr, pod_cidr, svc_cidr, bundle, os_image,
	k8s_version, cni, phase, status, generation, observed_generation, created_at, deleted_at, cni_version,
	control_planes, api_vip, target_bundle, owner_id, monitoring_wired, provider, network_name, ip_mode,
	net_gateway, net_dns, static_ips, cert_not_after, load_balancer_ip, gateway_wired,
	dns_domain, apps_domain, dns_wired, storage_disk_gb, storage_wired,
	etcd_db_bytes, etcd_db_in_use_bytes, etcd_quota_bytes, etcd_alarms, etcd_members,
	etcd_observed_at, etcd_defragged_at, etcd_snapshot_at, repair, repair_observed_at,
	vault_wired, registry_wired, registry_robot_not_after`

// etcdCols is the tail of clusterCols carrying domain.Cluster.Etcd, and etcdSet its assignment list
// for the two UPDATE statements. Kept as constants next to etcdArgs/scanEtcd so the column order,
// the parameter order and the scan order are edited in one place - seven positional columns is
// exactly where a hand-maintained UPDATE starts silently writing the wrong field.
const etcdSet = `etcd_db_bytes=$36, etcd_db_in_use_bytes=$37, etcd_quota_bytes=$38,
	etcd_alarms=$39, etcd_members=$40, etcd_observed_at=$41, etcd_defragged_at=$42,
	etcd_snapshot_at=$43, repair=$44, repair_observed_at=$45`

// updateArgs is the positional argument list shared by UpdateCluster and
// UpdateClusterUnlessSuperseded, whose SET clauses are identical (the guarded one only adds a WHERE
// term on a parameter it already binds). Shared so the two can never drift apart - they have to
// write the same row the same way.
func updateArgs(c *domain.Cluster) []any {
	args := append([]any{
		c.ID, c.Name, c.Size, c.PodCIDR, c.SvcCIDR, c.Bundle, c.OSImage,
		c.K8sVersion, c.CNI, string(c.Phase), c.Status, c.Generation, c.ObservedGeneration, c.DeletedAt,
		c.CNIVersion, c.ControlPlanes, c.APIVIP, c.TargetBundle, c.NetworkCIDR, c.OwnerID, c.MonitoringWired,
		c.InfraProvider(), c.NetworkName, c.IPMode, c.NetGateway, c.NetDNS, staticIPsJSON(c.StaticIPs),
		c.CertNotAfter, c.LoadBalancerIP, c.GatewayWired, c.DNSDomain, c.AppsDomain, c.DNSWired,
		c.StorageDiskGB, c.StorageWired,
	}, append(etcdArgs(c), repairArgs(c)...)...)
	// The integration columns are appended LAST (params $46-$48, after etcdSet's $45) so the
	// etcd/repair positional block stays put - the same "new columns go at the end" rule as the
	// migration's ADD COLUMN.
	return append(args, c.VaultWired, c.RegistryWired, c.RegistryRobotNotAfter)
}

// etcdArgs flattens domain.Cluster.Etcd into its seven columns. A nil status (never observed) writes
// zeroes and a NULL etcd_observed_at, which is what the due-scan reads as "observe this one".
func etcdArgs(c *domain.Cluster) []any {
	if c.Etcd == nil {
		return []any{int64(0), int64(0), int64(0), []string{}, 0, nil, nil}
	}
	alarms := c.Etcd.Alarms
	if alarms == nil {
		alarms = []string{} // the column is NOT NULL; a nil slice would marshal to NULL
	}
	var observedAt any
	if !c.Etcd.ObservedAt.IsZero() {
		observedAt = c.Etcd.ObservedAt
	}
	return []any{c.Etcd.DBBytes, c.Etcd.DBInUseBytes, c.Etcd.QuotaBytes, alarms, c.Etcd.Members,
		observedAt, c.Etcd.DefraggedAt}
}

// repairArgs flattens the snapshot cadence marker and the repair state into their three columns.
// The repair blob is stored whole because nothing queries inside it (see migrations/0032); the
// observation timestamp is lifted OUT of it into its own column purely so the due-scan's index is a
// btree on a timestamp rather than a JSONB expression.
func repairArgs(c *domain.Cluster) []any {
	if c.Repair == nil {
		return []any{c.EtcdSnapshotAt, []byte("{}"), nil}
	}
	blob, err := json.Marshal(c.Repair)
	if err != nil {
		// A repair state that cannot be marshalled is a programming error, not a runtime condition.
		// Writing "{}" loses the state rather than failing the whole cluster update - the next
		// observation rebuilds it from the health snapshot, which is where it came from anyway.
		blob = []byte("{}")
	}
	var observedAt any
	if !c.Repair.ObservedAt.IsZero() {
		observedAt = c.Repair.ObservedAt
	}
	return []any{c.EtcdSnapshotAt, blob, observedAt}
}

func (s *Store) CreateCluster(c *domain.Cluster) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	args := append([]any{
		c.ID, c.Name, c.Size, c.NetworkCIDR, c.PodCIDR, c.SvcCIDR, c.Bundle, c.OSImage,
		c.K8sVersion, c.CNI, string(c.Phase), c.Status, c.Generation, c.ObservedGeneration,
		c.CreatedAt, c.DeletedAt, c.CNIVersion, c.ControlPlanes, c.APIVIP, c.TargetBundle, c.OwnerID,
		c.MonitoringWired, c.InfraProvider(), c.NetworkName, c.IPMode, c.NetGateway, c.NetDNS,
		staticIPsJSON(c.StaticIPs), c.CertNotAfter, c.LoadBalancerIP, c.GatewayWired,
		c.DNSDomain, c.AppsDomain, c.DNSWired, c.StorageDiskGB, c.StorageWired,
	}, append(etcdArgs(c), repairArgs(c)...)...)
	// Last columns, params $47-$49 (see clusterCols / updateArgs).
	args = append(args, c.VaultWired, c.RegistryWired, c.RegistryRobotNotAfter)
	_, err = tx.Exec(ctx, `INSERT INTO clusters (`+clusterCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,
			$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43,
			$44,$45,$46,$47,$48,$49)`, args...)
	if err != nil {
		return fmt.Errorf("postgres: insert cluster: %w", err)
	}
	if err := writeChildren(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) UpdateCluster(c *domain.Cluster) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `UPDATE clusters SET
		name=$2, size=$3, pod_cidr=$4, svc_cidr=$5, bundle=$6, os_image=$7,
		k8s_version=$8, cni=$9, phase=$10, status=$11, generation=$12, observed_generation=$13,
		deleted_at=$14, cni_version=$15, control_planes=$16, api_vip=$17, target_bundle=$18,
		network_cidr=$19, owner_id=$20, monitoring_wired=$21, provider=$22, network_name=$23,
		ip_mode=$24, net_gateway=$25, net_dns=$26, static_ips=$27, cert_not_after=$28,
		load_balancer_ip=$29, gateway_wired=$30, dns_domain=$31, apps_domain=$32,
		dns_wired=$33, storage_disk_gb=$34, storage_wired=$35, `+etcdSet+`, vault_wired=$46,
		registry_wired=$47, registry_robot_not_after=$48 WHERE id=$1`,
		updateArgs(c)...)
	if err != nil {
		return fmt.Errorf("postgres: update cluster: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	// Children are fully owned by the cluster: replace them wholesale (idempotent).
	if _, err := tx.Exec(ctx, `DELETE FROM nodes WHERE cluster_id=$1`, c.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM node_pools WHERE cluster_id=$1`, c.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM node_disks WHERE cluster_id=$1`, c.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cluster_addons WHERE cluster_id=$1`, c.ID); err != nil {
		return err
	}
	if err := writeChildren(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateClusterUnlessSuperseded is UpdateCluster with a generation guard: the row is written only
// while its stored generation still equals c.Generation (the value the reconciler read). A
// concurrent edit or delete bumps the generation, so the guarded UPDATE matches no row and the
// reconciler's stale transition is refused - see the interface doc. Refusal is ErrConflict when the
// row still exists (superseded) and ErrNotFound when it's gone.
func (s *Store) UpdateClusterUnlessSuperseded(c *domain.Cluster) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `UPDATE clusters SET
		name=$2, size=$3, pod_cidr=$4, svc_cidr=$5, bundle=$6, os_image=$7,
		k8s_version=$8, cni=$9, phase=$10, status=$11, generation=$12, observed_generation=$13,
		deleted_at=$14, cni_version=$15, control_planes=$16, api_vip=$17, target_bundle=$18,
		network_cidr=$19, owner_id=$20, monitoring_wired=$21, provider=$22, network_name=$23,
		ip_mode=$24, net_gateway=$25, net_dns=$26, static_ips=$27, cert_not_after=$28,
		load_balancer_ip=$29, gateway_wired=$30, dns_domain=$31, apps_domain=$32,
		dns_wired=$33, storage_disk_gb=$34, storage_wired=$35, `+etcdSet+`, vault_wired=$46,
		registry_wired=$47, registry_robot_not_after=$48
		WHERE id=$1 AND generation=$12`,
		updateArgs(c)...)
	if err != nil {
		return fmt.Errorf("postgres: update cluster (guarded): %w", err)
	}
	if tag.RowsAffected() == 0 {
		// No row matched (id, generation). Distinguish "superseded" (row exists at a newer
		// generation) from "gone" so the reconciler logs the right thing.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM clusters WHERE id=$1)`, c.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return store.ErrConflict
		}
		return store.ErrNotFound
	}
	// Children are fully owned by the cluster: replace them wholesale (idempotent).
	if _, err := tx.Exec(ctx, `DELETE FROM nodes WHERE cluster_id=$1`, c.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM node_pools WHERE cluster_id=$1`, c.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM node_disks WHERE cluster_id=$1`, c.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cluster_addons WHERE cluster_id=$1`, c.ID); err != nil {
		return err
	}
	if err := writeChildren(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) GetCluster(id string) (*domain.Cluster, error) {
	ctx := context.Background()
	row := s.pool.QueryRow(ctx, `SELECT `+clusterCols+` FROM clusters WHERE id=$1`, id)
	c, err := scanCluster(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if err := s.loadChildren(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) ListClusters() ([]*domain.Cluster, error) {
	return s.queryClusters(`SELECT ` + clusterCols + ` FROM clusters ORDER BY created_at`)
}

func (s *Store) ClustersNeedingWork() ([]*domain.Cluster, error) {
	return s.queryClusters(`SELECT ` + clusterCols + ` FROM clusters
		WHERE phase NOT IN ('Deleted','Failed')
		  AND (phase <> 'Ready' OR observed_generation <> generation)`)
}

// ClustersDueCertRenewal returns Ready, converged clusters whose control-plane certificate expiry
// is unknown (never observed) or falls before cutoff (now + the renewal window). These are exactly
// the clusters the generation-driven ClustersNeedingWork misses - a Ready idle cluster is otherwise
// never re-reconciled - so the reconciler unions this set in when automatic rotation is enabled.
func (s *Store) ClustersDueCertRenewal(cutoff time.Time) ([]*domain.Cluster, error) {
	return s.queryClusters(`SELECT `+clusterCols+` FROM clusters
		WHERE phase = 'Ready' AND observed_generation = generation
		  AND (cert_not_after IS NULL OR cert_not_after < $1)`, cutoff)
}

// ClustersDueEtcdMaintenance returns Ready, converged clusters whose etcd backend store has never
// been observed or was last observed before cutoff (now - the observation interval). Same role as
// ClustersDueCertRenewal - a Ready idle cluster is otherwise never re-reconciled - with one
// difference that matters: certificate expiry is observed ONCE and then known, so that scan drains
// itself, while an etcd store drifts and this one re-fires every interval forever. That is why the
// interval is coarse (hours) and why the Ready-tick ranks etcd work last.
func (s *Store) ClustersDueEtcdMaintenance(cutoff time.Time) ([]*domain.Cluster, error) {
	return s.queryClusters(`SELECT `+clusterCols+` FROM clusters
		WHERE phase = 'Ready' AND observed_generation = generation
		  AND (etcd_observed_at IS NULL OR etcd_observed_at < $1)`, cutoff)
}

// ClustersDueEtcdSnapshot returns Ready, converged clusters that have never been snapshotted or were
// last snapshotted before cutoff (now - the snapshot interval). Same shape as the two scans above.
// Like the etcd one and unlike the certificate one, this scan never drains itself: a backup is due
// again the moment the interval elapses, forever, which is what makes the interval the real bound on
// how much a restore can lose.
func (s *Store) ClustersDueEtcdSnapshot(cutoff time.Time) ([]*domain.Cluster, error) {
	return s.queryClusters(`SELECT `+clusterCols+` FROM clusters
		WHERE phase = 'Ready' AND observed_generation = generation
		  AND (etcd_snapshot_at IS NULL OR etcd_snapshot_at < $1)`, cutoff)
}

// ClustersDueRepair returns Ready, converged clusters whose repair state has never been refreshed or
// was refreshed before cutoff. The cheapest of the four time-driven scans to SATISFY - the work
// behind it reads a health snapshot the health sweep already stored - which is why its interval is
// minutes where etcd's is hours.
func (s *Store) ClustersDueRepair(cutoff time.Time) ([]*domain.Cluster, error) {
	return s.queryClusters(`SELECT `+clusterCols+` FROM clusters
		WHERE phase = 'Ready' AND observed_generation = generation
		  AND (repair_observed_at IS NULL OR repair_observed_at < $1)`, cutoff)
}

func (s *Store) ListClustersByOwner(ownerID string) ([]*domain.Cluster, error) {
	return s.queryClusters(`SELECT `+clusterCols+` FROM clusters WHERE owner_id=$1 ORDER BY created_at`, ownerID)
}

// userCols is the read/insert column list. NOTE: UpdateUser does NOT derive its SET list from this
// - adding a column here and forgetting it there compiles cleanly and silently never persists on
// update. Change both.
const userCols = `id, username, password_hash, auth_source, email, display_name, is_admin, quotas, created_at`

func scanUser(row scanRow) (*domain.User, error) {
	var u domain.User
	var quotas []byte
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.AuthSource, &u.Email, &u.DisplayName,
		&u.IsAdmin, &quotas, &u.CreatedAt); err != nil {
		return nil, err
	}
	if len(quotas) > 0 {
		if err := json.Unmarshal(quotas, &u.Quotas); err != nil {
			return nil, fmt.Errorf("postgres: decode user quotas: %w", err)
		}
	}
	return &u, nil
}

// authSource normalizes a user's provenance for the NOT NULL column. Callers that predate directory
// auth - and every test that builds a domain.User literal - leave it empty, which means local; the
// column has no room for that ambiguity, so it is resolved here rather than at each call site.
func authSource(u *domain.User) string {
	if u.AuthSource == "" {
		return domain.AuthSourceLocal
	}
	return u.AuthSource
}

// groupSource does the same for a group's ownership.
func groupSource(g *domain.Group) string {
	if g.Source == "" {
		return domain.SourceLocal
	}
	return g.Source
}

// quotasJSON encodes a user's per-infrastructure grants for the quotas JSONB column. A nil map is
// stored as {} rather than NULL, so the column's NOT NULL default holds and reads never special-case.
func quotasJSON(q map[string]domain.ResourceQuota) ([]byte, error) {
	if q == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(q)
}

// loadMemberships attaches the group_memberships rows for one user (nil when ungrouped).
func (s *Store) loadMemberships(ctx context.Context, userID string) ([]domain.GroupMembership, error) {
	rows, err := s.pool.Query(ctx, `SELECT group_id, role FROM group_memberships WHERE user_id=$1 ORDER BY group_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.GroupMembership
	for rows.Next() {
		var m domain.GroupMembership
		var role string // scanned as text, like Phase (see scanCluster)
		if err := rows.Scan(&m.GroupID, &role); err != nil {
			return nil, err
		}
		m.Role = domain.GroupRole(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// writeMemberships replaces a user's membership set inside tx (delete-all + re-insert), the same
// idempotent sync the in-memory store gets for free by storing the slice on the user.
func writeMemberships(ctx context.Context, tx pgx.Tx, u *domain.User) error {
	if _, err := tx.Exec(ctx, `DELETE FROM group_memberships WHERE user_id=$1`, u.ID); err != nil {
		return fmt.Errorf("postgres: clear memberships: %w", err)
	}
	for _, m := range u.Memberships {
		if _, err := tx.Exec(ctx,
			`INSERT INTO group_memberships (user_id, group_id, role) VALUES ($1,$2,$3)`,
			u.ID, m.GroupID, string(m.Role)); err != nil {
			return fmt.Errorf("postgres: insert membership: %w", err)
		}
	}
	return nil
}

func (s *Store) CreateUser(u *domain.User) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	quotas, err := quotasJSON(u.Quotas)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO users (`+userCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		u.ID, u.Username, u.PasswordHash, authSource(u), u.Email, u.DisplayName, u.IsAdmin, quotas, u.CreatedAt)
	if isUniqueViolation(err) {
		return store.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("postgres: insert user: %w", err)
	}
	if err := writeMemberships(ctx, tx, u); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) GetUser(id string) (*domain.User, error) {
	ctx := context.Background()
	row := s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if u.Memberships, err = s.loadMemberships(ctx, u.ID); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) GetUserByUsername(username string) (*domain.User, error) {
	ctx := context.Background()
	row := s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username=$1`, username)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if u.Memberships, err = s.loadMemberships(ctx, u.ID); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) ListUsers() ([]*domain.User, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.User
	byID := make(map[string]*domain.User)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
		byID[u.ID] = u
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Attach memberships in one pass rather than a query per user.
	mrows, err := s.pool.Query(ctx, `SELECT user_id, group_id, role FROM group_memberships ORDER BY group_id`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var userID, role string
		var m domain.GroupMembership
		if err := mrows.Scan(&userID, &m.GroupID, &role); err != nil {
			return nil, err
		}
		m.Role = domain.GroupRole(role)
		if u := byID[userID]; u != nil {
			u.Memberships = append(u.Memberships, m)
		}
	}
	return out, mrows.Err()
}

func (s *Store) UpdateUser(u *domain.User) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	quotas, err := quotasJSON(u.Quotas)
	if err != nil {
		return err
	}
	// This SET list is written out rather than derived from userCols, so it must be kept in step
	// with it by hand. created_at is the one column deliberately absent (it is set once, at insert).
	tag, err := tx.Exec(ctx, `UPDATE users SET
		username=$2, password_hash=$3, auth_source=$4, email=$5, display_name=$6,
		is_admin=$7, quotas=$8 WHERE id=$1`,
		u.ID, u.Username, u.PasswordHash, authSource(u), u.Email, u.DisplayName, u.IsAdmin, quotas)
	if isUniqueViolation(err) {
		return store.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("postgres: update user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	if err := writeMemberships(ctx, tx, u); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteUser(id string) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM group_memberships WHERE user_id=$1`, id); err != nil {
		return fmt.Errorf("postgres: delete user memberships: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("postgres: delete user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return tx.Commit(ctx)
}

// Login throttle (see store.Store.LoginFailures, migrations/0022, internal/app/throttle.go).

func (s *Store) LoginFailures(scope, key string) (int, time.Time, error) {
	var count int
	var start time.Time
	err := s.pool.QueryRow(context.Background(),
		`SELECT failures, window_start FROM login_failures WHERE scope=$1 AND key=$2`, scope, key).
		Scan(&count, &start)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil // no record is a zero count, not an error
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("postgres: read login failures: %w", err)
	}
	return count, start, nil
}

// RecordLoginFailure increments the counter for (scope, key), restarting the window if the previous
// one has expired.
//
// The upsert does the whole decision in one statement on purpose: this runs on the unauthenticated
// login path from N api replicas at once, and a read-then-write would let a burst of concurrent
// attempts each read the same low count and undercount the burst - which is exactly the traffic
// shape the throttle exists to stop.
func (s *Store) RecordLoginFailure(scope, key string, window time.Duration) error {
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO login_failures (scope, key, failures, window_start)
		 VALUES ($1, $2, 1, now())
		 ON CONFLICT (scope, key) DO UPDATE SET
		   failures = CASE
		     WHEN login_failures.window_start < now() - $3::interval THEN 1
		     ELSE login_failures.failures + 1
		   END,
		   window_start = CASE
		     WHEN login_failures.window_start < now() - $3::interval THEN now()
		     ELSE login_failures.window_start
		   END`,
		scope, key, window)
	if err != nil {
		return fmt.Errorf("postgres: record login failure: %w", err)
	}
	return nil
}

func (s *Store) ResetLoginFailures(scope, key string) error {
	_, err := s.pool.Exec(context.Background(),
		`DELETE FROM login_failures WHERE scope=$1 AND key=$2`, scope, key)
	if err != nil {
		return fmt.Errorf("postgres: reset login failures: %w", err)
	}
	return nil
}

// PruneLoginFailures drops counters whose window has expired. The table is otherwise unbounded in
// the number of distinct usernames anyone has ever guessed at - which, on a public login endpoint,
// is an attacker-controlled number.
func (s *Store) PruneLoginFailures(window time.Duration) (int64, error) {
	tag, err := s.pool.Exec(context.Background(),
		`DELETE FROM login_failures WHERE window_start < now() - $1::interval`, window)
	if err != nil {
		return 0, fmt.Errorf("postgres: prune login failures: %w", err)
	}
	return tag.RowsAffected(), nil
}

const groupCols = `id, name, source, source_key, created_at`

func scanGroup(row scanRow) (*domain.Group, error) {
	var g domain.Group
	if err := row.Scan(&g.ID, &g.Name, &g.Source, &g.SourceKey, &g.CreatedAt); err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *Store) CreateGroup(g *domain.Group) error {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `INSERT INTO groups (`+groupCols+`) VALUES ($1,$2,$3,$4,$5)`,
		g.ID, g.Name, groupSource(g), g.SourceKey, g.CreatedAt)
	if isUniqueViolation(err) {
		return store.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("postgres: insert group: %w", err)
	}
	return nil
}

func (s *Store) GetGroup(id string) (*domain.Group, error) {
	row := s.pool.QueryRow(context.Background(), `SELECT `+groupCols+` FROM groups WHERE id=$1`, id)
	g, err := scanGroup(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return g, err
}

// GetGroupBySource resolves a directory group by the mapping rule that owns it. Backed by the
// partial unique index groups_source_key, which is also what serializes the boot-time seeding race
// between replicas (see migrations/0021).
func (s *Store) GetGroupBySource(source, sourceKey string) (*domain.Group, error) {
	if source == domain.SourceLocal || source == "" || sourceKey == "" {
		return nil, store.ErrNotFound // local groups share the empty key; not addressable this way
	}
	row := s.pool.QueryRow(context.Background(),
		`SELECT `+groupCols+` FROM groups WHERE source=$1 AND source_key=$2`, source, sourceKey)
	g, err := scanGroup(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return g, err
}

func (s *Store) ListGroups() ([]*domain.Group, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT `+groupCols+` FROM groups ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UpdateGroup changes a group's display name and NOTHING else.
//
// source/source_key are omitted on purpose - a group's ownership is immutable. If an update could
// set them, a rename would be able to hand an admin-managed group to a directory rule (or steal a
// directory group into local hands), which is exactly the capture this design refuses. The rename
// of a directory group's LABEL is legitimate and is what this supports (see ensureDirectoryGroups);
// changing who owns it is not. Do not "complete" this SET list.
func (s *Store) UpdateGroup(g *domain.Group) error {
	ctx := context.Background()
	tag, err := s.pool.Exec(ctx, `UPDATE groups SET name=$2 WHERE id=$1`, g.ID, g.Name)
	if isUniqueViolation(err) {
		return store.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("postgres: update group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteGroup(id string) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Drop any lingering memberships (the app layer removes them per-user first, but keep the store
	// self-consistent even if a caller deletes a group directly).
	if _, err := tx.Exec(ctx, `DELETE FROM group_memberships WHERE group_id=$1`, id); err != nil {
		return fmt.Errorf("postgres: delete group memberships: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM groups WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("postgres: delete group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return tx.Commit(ctx)
}

// isUniqueViolation reports whether err is a Postgres unique_violation (SQLSTATE 23505),
// mapped to store.ErrConflict so callers get a stable, backend-agnostic error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

const catalogCols = `id, owner_id, name, created_at`

func scanCatalog(row scanRow) (*domain.CustomCatalog, error) {
	var cc domain.CustomCatalog
	if err := row.Scan(&cc.ID, &cc.OwnerID, &cc.Name, &cc.CreatedAt); err != nil {
		return nil, err
	}
	return &cc, nil
}

// loadCatalogAddons attaches the custom_catalog_addons rows for one catalog, ordered by the
// authoring position the app layer pinned (name breaks ties).
func (s *Store) loadCatalogAddons(ctx context.Context, cc *domain.CustomCatalog) error {
	rows, err := s.pool.Query(ctx, `SELECT name, description, repo, chart, version, namespace, default_values
		FROM custom_catalog_addons WHERE catalog_id=$1 ORDER BY position, name`, cc.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	cc.Addons = nil
	for rows.Next() {
		var a domain.CustomAddon
		if err := rows.Scan(&a.Name, &a.Description, &a.Repo, &a.Chart, &a.Version, &a.Namespace, &a.Values); err != nil {
			return err
		}
		cc.Addons = append(cc.Addons, a)
	}
	return rows.Err()
}

// writeCatalogAddons rewrites a catalog's add-on child rows whole (caller clears existing rows in
// the same tx first); the slice index becomes position so a reload preserves authoring order.
func writeCatalogAddons(ctx context.Context, tx pgx.Tx, cc *domain.CustomCatalog) error {
	for i, a := range cc.Addons {
		if _, err := tx.Exec(ctx, `INSERT INTO custom_catalog_addons
			(catalog_id, name, description, repo, chart, version, namespace, default_values, position)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			cc.ID, a.Name, a.Description, a.Repo, a.Chart, a.Version, a.Namespace, a.Values, i); err != nil {
			return fmt.Errorf("postgres: insert custom catalog addon: %w", err)
		}
	}
	return nil
}

func (s *Store) CreateCustomCatalog(cc *domain.CustomCatalog) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO custom_catalogs (`+catalogCols+`) VALUES ($1,$2,$3,$4)`,
		cc.ID, cc.OwnerID, cc.Name, cc.CreatedAt)
	if isUniqueViolation(err) {
		return store.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("postgres: insert custom catalog: %w", err)
	}
	if err := writeCatalogAddons(ctx, tx, cc); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) GetCustomCatalog(id string) (*domain.CustomCatalog, error) {
	ctx := context.Background()
	row := s.pool.QueryRow(ctx, `SELECT `+catalogCols+` FROM custom_catalogs WHERE id=$1`, id)
	cc, err := scanCatalog(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.loadCatalogAddons(ctx, cc); err != nil {
		return nil, err
	}
	return cc, nil
}

func (s *Store) ListCustomCatalogs() ([]*domain.CustomCatalog, error) {
	return s.queryCatalogs(`SELECT ` + catalogCols + ` FROM custom_catalogs ORDER BY created_at`)
}

func (s *Store) ListCustomCatalogsByOwner(ownerID string) ([]*domain.CustomCatalog, error) {
	return s.queryCatalogs(`SELECT `+catalogCols+` FROM custom_catalogs WHERE owner_id=$1 ORDER BY created_at`, ownerID)
}

func (s *Store) queryCatalogs(sql string, args ...any) ([]*domain.CustomCatalog, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	var out []*domain.CustomCatalog
	for rows.Next() {
		cc, err := scanCatalog(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, cc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Load each catalog's add-ons after draining the list query (can't run a nested query on an open
	// rows cursor over the same pool connection).
	for _, cc := range out {
		if err := s.loadCatalogAddons(ctx, cc); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) UpdateCustomCatalog(cc *domain.CustomCatalog) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE custom_catalogs SET name=$2 WHERE id=$1`, cc.ID, cc.Name)
	if isUniqueViolation(err) {
		return store.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("postgres: update custom catalog: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM custom_catalog_addons WHERE catalog_id=$1`, cc.ID); err != nil {
		return fmt.Errorf("postgres: clear custom catalog addons: %w", err)
	}
	if err := writeCatalogAddons(ctx, tx, cc); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteCustomCatalog(id string) error {
	ctx := context.Background()
	// custom_catalog_addons cascade on the FK; a single DELETE suffices.
	tag, err := s.pool.Exec(ctx, `DELETE FROM custom_catalogs WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("postgres: delete custom catalog: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) SaveSecret(clusterID string, kind domain.SecretKind, ciphertext []byte) error {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `INSERT INTO secrets (cluster_id, kind, ciphertext)
		VALUES ($1,$2,$3)
		ON CONFLICT (cluster_id, kind) DO UPDATE SET ciphertext = EXCLUDED.ciphertext`,
		clusterID, string(kind), ciphertext)
	return err
}

func (s *Store) GetSecret(clusterID string, kind domain.SecretKind) ([]byte, error) {
	ctx := context.Background()
	var ct []byte
	err := s.pool.QueryRow(ctx, `SELECT ciphertext FROM secrets WHERE cluster_id=$1 AND kind=$2`,
		clusterID, string(kind)).Scan(&ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return ct, err
}

func (s *Store) RecordOperation(op *domain.Operation) error {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `INSERT INTO operations
		(id, cluster_id, kind, summary, detail, generation, status, actor_id, actor_username, started_at, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		op.ID, op.ClusterID, string(op.Kind), op.Summary, op.Detail, op.Generation,
		string(op.Status), op.ActorID, op.ActorUsername, op.StartedAt, op.FinishedAt)
	if err != nil {
		return fmt.Errorf("postgres: insert operation: %w", err)
	}
	return nil
}

func (s *Store) ListOperations(clusterID string) ([]*domain.Operation, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT id, cluster_id, kind, summary, detail, generation,
		status, actor_id, actor_username, started_at, finished_at FROM operations
		WHERE cluster_id=$1 ORDER BY started_at DESC`, clusterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Operation
	for rows.Next() {
		var op domain.Operation
		var kind, status string
		if err := rows.Scan(&op.ID, &op.ClusterID, &kind, &op.Summary, &op.Detail,
			&op.Generation, &status, &op.ActorID, &op.ActorUsername, &op.StartedAt, &op.FinishedAt); err != nil {
			return nil, err
		}
		op.Kind = domain.OperationKind(kind)
		op.Status = domain.OperationStatus(status)
		out = append(out, &op)
	}
	return out, rows.Err()
}

func (s *Store) CompleteOperations(clusterID string, throughGeneration int64, at time.Time) error {
	ctx := context.Background()
	// The exempt kinds (an open SSH session, any platform-initiated maintenance/repair) finish
	// themselves via CompleteOperation; the generation sweep must not close one that is still running
	// - see domain.OperationKind.SweepExempt.
	exempt := domain.SweepExemptKinds()
	strs := make([]string, len(exempt))
	for i, k := range exempt {
		strs[i] = string(k)
	}
	_, err := s.pool.Exec(ctx, `UPDATE operations SET status=$1, finished_at=$2
		WHERE cluster_id=$3 AND status=$4 AND generation<=$5 AND kind <> ALL($6)`,
		string(domain.OpCompleted), at, clusterID, string(domain.OpInProgress), throughGeneration, strs)
	return err
}

func (s *Store) CompleteOperation(id, detail string, at time.Time) error {
	ctx := context.Background()
	// COALESCE-style: an empty detail leaves the stored one untouched, so a session with no captured
	// commands doesn't blank a detail some other writer set.
	tag, err := s.pool.Exec(ctx, `UPDATE operations
		SET status=$1, finished_at=$2, detail=CASE WHEN $3='' THEN detail ELSE $3 END
		WHERE id=$4`,
		string(domain.OpCompleted), at, detail, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) SaveMetrics(snapshot *domain.MetricsSnapshot) error {
	ctx := context.Background()
	nodes, err := json.Marshal(snapshot.Nodes)
	if err != nil {
		return fmt.Errorf("postgres: marshal metrics: %w", err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO cluster_metrics (cluster_id, collected_at, nodes)
		VALUES ($1,$2,$3)
		ON CONFLICT (cluster_id) DO UPDATE SET collected_at = EXCLUDED.collected_at, nodes = EXCLUDED.nodes`,
		snapshot.ClusterID, snapshot.CollectedAt, nodes)
	return err
}

func (s *Store) GetMetrics(clusterID string) (*domain.MetricsSnapshot, error) {
	ctx := context.Background()
	var snap domain.MetricsSnapshot
	var nodes []byte
	err := s.pool.QueryRow(ctx, `SELECT cluster_id, collected_at, nodes
		FROM cluster_metrics WHERE cluster_id=$1`, clusterID).Scan(&snap.ClusterID, &snap.CollectedAt, &nodes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(nodes, &snap.Nodes); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal metrics: %w", err)
	}
	return &snap, nil
}

func (s *Store) SaveHealth(snapshot *domain.HealthSnapshot) error {
	ctx := context.Background()
	checks, err := json.Marshal(snapshot.Checks)
	if err != nil {
		return fmt.Errorf("postgres: marshal health checks: %w", err)
	}
	nodes, err := json.Marshal(snapshot.Nodes)
	if err != nil {
		return fmt.Errorf("postgres: marshal health nodes: %w", err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO cluster_health (cluster_id, collected_at, status, checks, nodes)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (cluster_id) DO UPDATE SET
			collected_at = EXCLUDED.collected_at, status = EXCLUDED.status,
			checks = EXCLUDED.checks, nodes = EXCLUDED.nodes`,
		snapshot.ClusterID, snapshot.CollectedAt, snapshot.Status, checks, nodes)
	return err
}

func (s *Store) GetHealth(clusterID string) (*domain.HealthSnapshot, error) {
	ctx := context.Background()
	var snap domain.HealthSnapshot
	var checks, nodes []byte
	err := s.pool.QueryRow(ctx, `SELECT cluster_id, collected_at, status, checks, nodes
		FROM cluster_health WHERE cluster_id=$1`, clusterID).
		Scan(&snap.ClusterID, &snap.CollectedAt, &snap.Status, &checks, &nodes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(checks, &snap.Checks); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal health checks: %w", err)
	}
	if err := json.Unmarshal(nodes, &snap.Nodes); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal health nodes: %w", err)
	}
	return &snap, nil
}

// snapshotCols is the METADATA column list - deliberately not including `payload`. Every read path
// but one wants the list without the bytes, and a SELECT * here would quietly turn "show me this
// cluster's backups" into moving hundreds of megabytes through the worker.
const snapshotCols = `id, cluster_id, taken_at, revision, hash, size_bytes, k8s_version, node_name`

func (s *Store) SaveEtcdSnapshot(snap *domain.EtcdSnapshot, sealed []byte) error {
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO etcd_snapshots (`+snapshotCols+`, payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		snap.ID, snap.ClusterID, snap.TakenAt, snap.Revision, int64(snap.Hash), snap.SizeBytes,
		snap.K8sVersion, snap.NodeName, sealed)
	if err != nil {
		return fmt.Errorf("postgres: insert etcd snapshot: %w", err)
	}
	return nil
}

func (s *Store) ListEtcdSnapshots(clusterID string) ([]domain.EtcdSnapshot, error) {
	rows, err := s.pool.Query(context.Background(),
		`SELECT `+snapshotCols+` FROM etcd_snapshots WHERE cluster_id=$1 ORDER BY taken_at DESC`, clusterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EtcdSnapshot
	for rows.Next() {
		var snap domain.EtcdSnapshot
		var hash int64
		if err := rows.Scan(&snap.ID, &snap.ClusterID, &snap.TakenAt, &snap.Revision, &hash,
			&snap.SizeBytes, &snap.K8sVersion, &snap.NodeName); err != nil {
			return nil, err
		}
		snap.Hash = uint32(hash)
		out = append(out, snap)
	}
	return out, rows.Err()
}

// GetEtcdSnapshotPayload is the only call that returns snapshot bytes, and only the worker's restore
// path makes it. The payload is sealed (secrets.Box) - it is the cluster's entire Secret set plus
// its CA key - and no API handler is wired to this for that reason.
func (s *Store) GetEtcdSnapshotPayload(id string) ([]byte, error) {
	var payload []byte
	err := s.pool.QueryRow(context.Background(), `SELECT payload FROM etcd_snapshots WHERE id=$1`, id).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return payload, err
}

func (s *Store) DeleteEtcdSnapshot(id string) error {
	_, err := s.pool.Exec(context.Background(), `DELETE FROM etcd_snapshots WHERE id=$1`, id)
	return err
}

// --- helpers ---------------------------------------------------------------

func (s *Store) queryClusters(sql string, args ...any) ([]*domain.Cluster, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	var clusters []*domain.Cluster
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		clusters = append(clusters, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range clusters { // load children after the rows are closed
		if err := s.loadChildren(ctx, c); err != nil {
			return nil, err
		}
	}
	return clusters, nil
}

// scanRow is satisfied by both pgx.Row and pgx.Rows.
type scanRow interface{ Scan(dest ...any) error }

func scanCluster(row scanRow) (*domain.Cluster, error) {
	var c domain.Cluster
	var phase string
	var etcd domain.EtcdStatus
	var etcdObservedAt *time.Time
	var repairBlob []byte
	var repairObservedAt *time.Time
	if err := row.Scan(&c.ID, &c.Name, &c.Size, &c.NetworkCIDR, &c.PodCIDR, &c.SvcCIDR,
		&c.Bundle, &c.OSImage, &c.K8sVersion, &c.CNI, &phase, &c.Status, &c.Generation,
		&c.ObservedGeneration, &c.CreatedAt, &c.DeletedAt, &c.CNIVersion,
		&c.ControlPlanes, &c.APIVIP, &c.TargetBundle, &c.OwnerID, &c.MonitoringWired,
		&c.Provider, &c.NetworkName, &c.IPMode, &c.NetGateway, &c.NetDNS, &c.StaticIPs,
		&c.CertNotAfter, &c.LoadBalancerIP, &c.GatewayWired,
		&c.DNSDomain, &c.AppsDomain, &c.DNSWired, &c.StorageDiskGB, &c.StorageWired,
		&etcd.DBBytes, &etcd.DBInUseBytes, &etcd.QuotaBytes, &etcd.Alarms, &etcd.Members,
		&etcdObservedAt, &etcd.DefraggedAt,
		&c.EtcdSnapshotAt, &repairBlob, &repairObservedAt, &c.VaultWired,
		&c.RegistryWired, &c.RegistryRobotNotAfter); err != nil {
		return nil, err
	}
	c.Phase = domain.Phase(phase)
	if len(c.StaticIPs) == 0 {
		c.StaticIPs = nil // keep the empty JSONB '{}' indistinguishable from unset
	}
	// A NULL etcd_observed_at is the "never observed" signal (the columns around it are NOT NULL and
	// default to zero), so it - not the sizes - decides whether the cluster carries an EtcdStatus at
	// all. Leaving c.Etcd nil is what makes the observation due on the next Ready tick.
	if etcdObservedAt != nil {
		etcd.ObservedAt = *etcdObservedAt
		if len(etcd.Alarms) == 0 {
			etcd.Alarms = nil // keep the empty TEXT[] '{}' indistinguishable from unset
		}
		c.Etcd = &etcd
	}
	// Same "NULL means never observed" convention as etcd above, and for the same reason: the blob
	// defaults to '{}', which unmarshals into a perfectly valid empty ClusterRepair and would make
	// every cluster look already-observed. The timestamp column is the signal, not the contents.
	if repairObservedAt != nil {
		var r domain.ClusterRepair
		if len(repairBlob) > 0 {
			if err := json.Unmarshal(repairBlob, &r); err != nil {
				return nil, fmt.Errorf("postgres: decode repair state: %w", err)
			}
		}
		r.ObservedAt = *repairObservedAt
		c.Repair = &r
	}
	return &c, nil
}

// staticIPsJSON keeps the static_ips column NOT NULL: a nil map marshals to JSON null via pgx,
// so write the empty object instead.
func staticIPsJSON(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (s *Store) loadChildren(ctx context.Context, c *domain.Cluster) error {
	nrows, err := s.pool.Query(ctx, `SELECT id, role, vm_name, pool, ip, mac, phase, image
		FROM nodes WHERE cluster_id=$1 ORDER BY role, vm_name`, c.ID)
	if err != nil {
		return err
	}
	c.Nodes = nil
	for nrows.Next() {
		var n domain.Node
		var role string
		if err := nrows.Scan(&n.ID, &role, &n.VMName, &n.Pool, &n.IP, &n.MAC, &n.Phase, &n.Image); err != nil {
			nrows.Close()
			return err
		}
		n.Role = domain.Role(role)
		c.Nodes = append(c.Nodes, n)
	}
	nrows.Close()
	if err := nrows.Err(); err != nil {
		return err
	}

	// ORDER BY position preserves the user's pool order, which is what DesiredNodes walks to mint VM
	// names - same reasoning as cluster_addons below.
	prows, err := s.pool.Query(ctx, `SELECT name, size, desired_workers, disk_gb
		FROM node_pools WHERE cluster_id=$1 ORDER BY position, name`, c.ID)
	if err != nil {
		return err
	}
	c.NodePools = nil
	for prows.Next() {
		var p domain.NodePool
		if err := prows.Scan(&p.Name, &p.Size, &p.DesiredWorkers, &p.DiskGB); err != nil {
			prows.Close()
			return err
		}
		c.NodePools = append(c.NodePools, p)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return err
	}

	// Extra disks, keyed on the node's VM name (see domain.NodeDisk). Ordered by (vm_name, name) so
	// a reload is stable; unlike pools and add-ons their order carries no meaning to converge, but a
	// deterministic one keeps the UI and the diffs steady.
	drows, err := s.pool.Query(ctx, `SELECT vm_name, name, size_gb, mount_path, fs_type, phase, wwn, device_id
		FROM node_disks WHERE cluster_id=$1 ORDER BY vm_name, name`, c.ID)
	if err != nil {
		return err
	}
	c.NodeDisks = nil
	for drows.Next() {
		var d domain.NodeDisk
		if err := drows.Scan(&d.VMName, &d.Name, &d.SizeGB, &d.MountPath, &d.FSType, &d.Phase, &d.WWN, &d.DeviceID); err != nil {
			drows.Close()
			return err
		}
		c.NodeDisks = append(c.NodeDisks, d)
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return err
	}

	// ORDER BY position preserves the install order the app layer pinned (resolveAddons: bundled
	// platform add-ons first, kube-prometheus-stack ahead of any add-on that depends on its CRDs).
	// name is a stable tiebreak for legacy rows that predate the position column (all default 0).
	arows, err := s.pool.Query(ctx, `SELECT name, version, phase, values_override,
		catalog_id, chart, repo, namespace, description
		FROM cluster_addons WHERE cluster_id=$1 ORDER BY position, name`, c.ID)
	if err != nil {
		return err
	}
	c.Addons = nil
	for arows.Next() {
		var a domain.Addon
		if err := arows.Scan(&a.Name, &a.Version, &a.Phase, &a.ValuesOverride,
			&a.CatalogID, &a.Chart, &a.Repo, &a.Namespace, &a.Description); err != nil {
			arows.Close()
			return err
		}
		c.Addons = append(c.Addons, a)
	}
	arows.Close()
	return arows.Err()
}

// writeChildren inserts a cluster's nodes, node pools and add-ons (callers clear existing rows first).
func writeChildren(ctx context.Context, tx pgx.Tx, c *domain.Cluster) error {
	for _, n := range c.Nodes {
		if _, err := tx.Exec(ctx, `INSERT INTO nodes (id, cluster_id, role, vm_name, pool, ip, mac, phase, image)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			n.ID, c.ID, string(n.Role), n.VMName, n.Pool, n.IP, n.MAC, n.Phase, n.Image); err != nil {
			return fmt.Errorf("postgres: insert node: %w", err)
		}
	}
	// Like the add-ons below, the slice index is persisted as position so a reload preserves the
	// user's pool order.
	for i, p := range c.NodePools {
		if _, err := tx.Exec(ctx, `INSERT INTO node_pools (cluster_id, name, size, desired_workers, position, disk_gb)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			c.ID, p.Name, p.Size, p.DesiredWorkers, i, p.DiskGB); err != nil {
			return fmt.Errorf("postgres: insert node pool: %w", err)
		}
	}
	for i, d := range c.NodeDisks {
		if _, err := tx.Exec(ctx, `INSERT INTO node_disks
			(cluster_id, vm_name, name, size_gb, mount_path, fs_type, phase, wwn, device_id, position)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			c.ID, d.VMName, d.Name, d.SizeGB, d.MountPath, d.FSType, d.Phase, d.WWN, d.DeviceID, i); err != nil {
			return fmt.Errorf("postgres: insert node disk: %w", err)
		}
	}
	// Persist the slice index as position so a reload preserves install order (see the ORDER BY in
	// load). Callers clear existing rows first, so positions are rewritten whole each save.
	for i, a := range c.Addons {
		if _, err := tx.Exec(ctx, `INSERT INTO cluster_addons
			(cluster_id, name, version, phase, values_override, position, catalog_id, chart, repo, namespace, description)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			c.ID, a.Name, a.Version, a.Phase, a.ValuesOverride, i,
			a.CatalogID, a.Chart, a.Repo, a.Namespace, a.Description); err != nil {
			return fmt.Errorf("postgres: insert addon: %w", err)
		}
	}
	return nil
}

// migrate applies migrations/*.sql that haven't been applied yet, tracked in schema_migrations.
//
// The whole sweep runs under an advisory lock because every api and worker replica migrates on
// boot, and they boot together: without it two replicas both see a version as unapplied and both
// apply it, and the loser dies on a duplicate-object error (which, at startup, is a crash loop).
// The lock makes the second replica wait, then find nothing to do.
func migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	return WithAdvisoryLock(ctx, pool, "schema-migrate", func() error {
		return migrateLocked(ctx, pool, dir)
	})
}

func migrateLocked(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("postgres: migrator init: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("postgres: read migrations dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		version := strings.TrimSuffix(f, ".sql")
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlBytes, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return err
		}
		// Simple protocol so multi-statement migration files run in one implicit transaction.
		if _, err := pool.Exec(ctx, string(sqlBytes), pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("postgres: apply %s: %w", version, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			return err
		}
	}
	return nil
}
