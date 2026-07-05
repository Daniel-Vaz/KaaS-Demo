// Package store defines the persistence seam and an in-memory implementation.
//
// The real implementation is Postgres (single source of truth), driven
// through this same interface. The in-memory store lets the control-plane logic run
// and be tested without a database.
package store

import (
	"errors"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a unique constraint would be violated (e.g. a duplicate username).
	ErrConflict = errors.New("conflict")
)

// Store is the persistence seam. Swap Memory for a Postgres impl behind this.
type Store interface {
	CreateCluster(c *domain.Cluster) error
	GetCluster(id string) (*domain.Cluster, error)
	ListClusters() ([]*domain.Cluster, error)
	// ListClustersByOwner returns only the clusters owned by ownerID - the per-tenant view. The
	// reconciler still uses ListClusters (platform-wide) so tenancy never changes the control loop.
	ListClustersByOwner(ownerID string) ([]*domain.Cluster, error)
	UpdateCluster(c *domain.Cluster) error

	// UpdateClusterUnlessSuperseded persists a reconciler-computed transition, but ONLY while the
	// cluster's desired generation is still the one the reconciler read (c.Generation). The
	// reconciler never changes generation - only the API does, on an edit or (critically) a delete,
	// which flips Phase to Deleting and bumps the generation. A slow phase step (helm --wait can run
	// for minutes) leaves a wide window in which such an intent can land after the reconciler read
	// its copy; a blind UpdateCluster would then clobber it and resurrect a cluster the user asked
	// to tear down. This refuses that write with ErrConflict instead (ErrNotFound if the row is
	// gone), so the reconciler drops its stale transition and the next tick re-reads the fresh
	// desired state (Deleting) and converges to it. Only the reconciler uses this; the API keeps the
	// blind UpdateCluster because it IS the authority on desired state.
	UpdateClusterUnlessSuperseded(c *domain.Cluster) error

	// ClustersNeedingWork returns clusters the reconciler must act on (a non-terminal phase, or a
	// Ready cluster whose observed generation trails its desired generation).
	ClustersNeedingWork() ([]*domain.Cluster, error)
	// ClustersDueCertRenewal returns Ready, converged clusters whose control-plane certificate expiry
	// is unknown or falls before cutoff - the time-driven work that ClustersNeedingWork can't see. The
	// reconciler unions this in only when automatic certificate rotation is enabled.
	ClustersDueCertRenewal(cutoff time.Time) ([]*domain.Cluster, error)
	// ClustersDueEtcdMaintenance returns Ready, converged clusters whose etcd backend store was last
	// observed before cutoff (now - KAAS_ETCD_OBSERVE_INTERVAL), or never. Same shape as the cert
	// scan and unioned in the same way, only when etcd maintenance is enabled.
	ClustersDueEtcdMaintenance(cutoff time.Time) ([]*domain.Cluster, error)
	// ClustersDueEtcdSnapshot returns Ready, converged clusters last snapshotted before cutoff
	// (now - KAAS_ETCD_SNAPSHOT_INTERVAL), or never. Same shape and same union as the two scans
	// above, only when snapshots are enabled.
	ClustersDueEtcdSnapshot(cutoff time.Time) ([]*domain.Cluster, error)
	// ClustersDueRepair returns Ready, converged clusters whose repair state was last refreshed
	// before cutoff, or never. Same shape again - but the cheapest of the four scans to satisfy,
	// because the observation behind it reads a health snapshot the health sweep already stored
	// rather than talking to the cluster.
	ClustersDueRepair(cutoff time.Time) ([]*domain.Cluster, error)

	// Users are tenant accounts - local ones with a bcrypt password, or directory ones provisioned
	// on first login (see internal/auth, internal/authn, docs/architecture.md). CreateUser and
	// UpdateUser return ErrConflict on a duplicate username. GetUser/GetUserByUsername return
	// ErrNotFound when absent.
	CreateUser(u *domain.User) error
	GetUser(id string) (*domain.User, error)
	GetUserByUsername(username string) (*domain.User, error)
	ListUsers() ([]*domain.User, error)
	UpdateUser(u *domain.User) error
	DeleteUser(id string) error

	// Login throttling: failed-attempt counters keyed by (scope, key), where scope is "user" (the
	// username) or "ip" (the client address). Shared across api replicas, so the allowance is
	// platform-wide rather than per-replica - see internal/app/throttle.go and migrations/0022.
	//
	// LoginFailures returns the current count and when its window opened; a zero count and zero
	// time mean "no record", never an error. RecordLoginFailure increments within the live window
	// or starts a new one at `window`; ResetLoginFailures clears the counter on success.
	LoginFailures(scope, key string) (count int, windowStart time.Time, err error)
	RecordLoginFailure(scope, key string, window time.Duration) error
	ResetLoginFailures(scope, key string) error

	// Groups are teams, owned either by admins or by a directory mapping rule (see
	// internal/domain.Group). CreateGroup and UpdateGroup return ErrConflict on a duplicate name.
	// GetGroup returns ErrNotFound when absent.
	CreateGroup(g *domain.Group) error
	GetGroup(id string) (*domain.Group, error)
	// GetGroupBySource finds a directory-owned group by the mapping rule that owns it, so boot-time
	// seeding is keyed on the rule rather than on the display name - renaming a rule's `group:`
	// relabels the existing group instead of forking a new one. Returns ErrNotFound when absent.
	GetGroupBySource(source, sourceKey string) (*domain.Group, error)
	ListGroups() ([]*domain.Group, error)
	UpdateGroup(g *domain.Group) error
	DeleteGroup(id string) error

	// CustomCatalogs are user-owned collections of self-defined add-ons (see
	// internal/domain.CustomCatalog). Create/Update return ErrConflict on a duplicate (owner, name);
	// Get returns ErrNotFound when absent. Update rewrites the add-on child rows whole. The catalog's
	// Addons are loaded/persisted with it.
	CreateCustomCatalog(cc *domain.CustomCatalog) error
	GetCustomCatalog(id string) (*domain.CustomCatalog, error)
	ListCustomCatalogs() ([]*domain.CustomCatalog, error)
	ListCustomCatalogsByOwner(ownerID string) ([]*domain.CustomCatalog, error)
	UpdateCustomCatalog(cc *domain.CustomCatalog) error
	DeleteCustomCatalog(id string) error

	SaveSecret(clusterID string, kind domain.SecretKind, ciphertext []byte) error
	GetSecret(clusterID string, kind domain.SecretKind) ([]byte, error)

	// Operations are the per-cluster action/audit history (see docs/architecture.md).
	// RecordOperation appends one; ListOperations returns them newest-first; CompleteOperations
	// marks every still-in-progress operation with generation <= throughGeneration finished at
	// `at` - the reconciler calls it once a cluster has converged to a generation. It deliberately
	// skips request-driven kinds (domain.OpSSH), which have no generation and finish on their own.
	RecordOperation(op *domain.Operation) error
	ListOperations(clusterID string) ([]*domain.Operation, error)
	CompleteOperations(clusterID string, throughGeneration int64, at time.Time) error
	// CompleteOperation finishes ONE operation by id, setting its detail (empty leaves it unchanged),
	// status=completed and finished_at=at. Used for request-driven operations (an SSH session) that
	// the reconciler's generation sweep does not cover.
	CompleteOperation(id, detail string, at time.Time) error

	// Metrics is the latest resource-usage snapshot for a cluster (live telemetry, not desired
	// state). SaveMetrics upserts the newest reading; GetMetrics returns it, or ErrNotFound if
	// none has been collected yet. See internal/metrics.
	SaveMetrics(snapshot *domain.MetricsSnapshot) error
	GetMetrics(clusterID string) (*domain.MetricsSnapshot, error)

	// Health is the latest health snapshot for a cluster (live telemetry, not desired state).
	// SaveHealth upserts the newest reading; GetHealth returns it, or ErrNotFound if none has been
	// evaluated yet. See internal/health.
	SaveHealth(snapshot *domain.HealthSnapshot) error
	GetHealth(clusterID string) (*domain.HealthSnapshot, error)

	// EtcdSnapshots are the platform's periodic control-plane backups (see domain.EtcdSnapshot).
	// Unlike every other store method here, the payload is the entire cluster's Secrets in plaintext
	// plus its CA key, so it arrives ALREADY SEALED (secrets.Box) and is never returned to anything
	// but the worker's restore path.
	//
	// SaveEtcdSnapshot appends one. ListEtcdSnapshots returns a cluster's snapshot METADATA only,
	// newest first, and deliberately never the payloads - the portal and the retention sweep both
	// want the list, and neither should be moving megabytes to get it. GetEtcdSnapshotPayload is the
	// one call that returns bytes, and only the restore path makes it. DeleteEtcdSnapshot drops one.
	SaveEtcdSnapshot(snap *domain.EtcdSnapshot, sealed []byte) error
	ListEtcdSnapshots(clusterID string) ([]domain.EtcdSnapshot, error)
	GetEtcdSnapshotPayload(id string) ([]byte, error)
	DeleteEtcdSnapshot(id string) error

	// WithLock runs fn while holding the named lock, excluding every other holder of that name -
	// including one in ANOTHER PROCESS. It exists because the API is horizontally scaled: cluster
	// admission (quota + IPAM in internal/app) is a read-then-write, and two API replicas serving
	// two creates against the same "before" snapshot would each pass a check the pair of them
	// jointly violates - double-allocating a node CIDR, or overshooting a tenant's quota. Postgres
	// implements it with a session advisory lock (a real cross-process mutex); the in-memory store
	// with a plain mutex (one process by definition). Locks are held for one short critical
	// section, never across a reconcile step.
	WithLock(name string, fn func() error) error
}

// Lock names (see Store.WithLock). Keep them few and coarse: they serialize control-plane writes.
//
// THEY DO NOT NEST. Both implementations make a nested acquire of the same name a hang, not an
// error: Memory.WithLock is a plain non-reentrant mutex, and the Postgres one takes a FRESH pool
// connection per call, so a nested pg_advisory_lock arrives from a different session and blocks
// forever. Anything called from inside a WithLock must be a ...Locked variant that does not take
// one itself (the pattern App.ensureAdmin/ensureAdminLocked follows).
const (
	// LockAdmission serializes the capacity/IPAM admission decision for cluster create and edit -
	// and every write to a user row, because the users table IS the quota ledger: Store.UpdateUser
	// rewrites the whole row, so an unserialized quota grant and directory login would silently
	// overwrite each other. See internal/app.updateUserLocked / syncDirectoryUserLocked.
	LockAdmission = "cluster-admission"
	// LockUserSeed serializes the idempotent boot-time seeding every replica performs: the admin
	// account, and the directory's mapping groups. Kept separate from LockAdmission because it is
	// boot-only - sharing them would let a slow login stall a rolling restart.
	LockUserSeed = "user-seed"
)

// Login-throttle scopes (see Store.LoginFailures).
const (
	ThrottleScopeUser = "user" // keyed on the username; protects one directory account from lockout
	ThrottleScopeIP   = "ip"   // keyed on the client address; catches one source spraying many names
)

// Memory is a goroutine-safe in-memory Store. It hands out deep copies so the
// reconciler and API can't race on shared pointers.
type Memory struct {
	mu         sync.RWMutex
	locks      sync.Map // lock name -> *sync.Mutex (see WithLock)
	clusters   map[string]domain.Cluster
	users      map[string]domain.User            // key: user ID
	groups     map[string]domain.Group           // key: group ID
	catalogs   map[string]domain.CustomCatalog   // key: catalog ID
	secrets    map[string][]byte                 // key: clusterID + "/" + kind
	operations map[string][]domain.Operation     // key: clusterID, append-ordered
	metrics    map[string]domain.MetricsSnapshot // key: clusterID, latest snapshot only
	health     map[string]domain.HealthSnapshot  // key: clusterID, latest snapshot only
	logins     map[string]loginFailure           // key: scope + "/" + key (see LoginFailures)
	// Etcd snapshots are split in two on purpose, mirroring the Postgres split: metadata is listed
	// constantly (retention, the portal, the restore decision) and the sealed payload is read once
	// per recovery, so keeping them in one map would make every listing copy megabytes.
	snapshots    map[string][]domain.EtcdSnapshot // key: clusterID
	snapshotData map[string][]byte                // key: snapshot ID -> sealed payload
}

// loginFailure is one throttle counter and the window it is counted in.
type loginFailure struct {
	failures    int
	windowStart time.Time
}

func NewMemory() *Memory {
	return &Memory{
		clusters:     make(map[string]domain.Cluster),
		users:        make(map[string]domain.User),
		groups:       make(map[string]domain.Group),
		catalogs:     make(map[string]domain.CustomCatalog),
		secrets:      make(map[string][]byte),
		operations:   make(map[string][]domain.Operation),
		metrics:      make(map[string]domain.MetricsSnapshot),
		health:       make(map[string]domain.HealthSnapshot),
		logins:       make(map[string]loginFailure),
		snapshots:    make(map[string][]domain.EtcdSnapshot),
		snapshotData: make(map[string][]byte),
	}
}

func throttleKey(scope, key string) string { return scope + "/" + key }

func (m *Memory) LoginFailures(scope, key string) (int, time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f := m.logins[throttleKey(scope, key)]
	return f.failures, f.windowStart, nil
}

func (m *Memory) RecordLoginFailure(scope, key string, window time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := throttleKey(scope, key)
	f, ok := m.logins[k]
	// A failure outside the live window starts a fresh one rather than adding to a stale count -
	// otherwise two failures a month apart would eventually trip the throttle.
	if !ok || time.Since(f.windowStart) > window {
		m.logins[k] = loginFailure{failures: 1, windowStart: time.Now()}
		return nil
	}
	f.failures++
	m.logins[k] = f
	return nil
}

func (m *Memory) ResetLoginFailures(scope, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.logins, throttleKey(scope, key))
	return nil
}

// WithLock runs fn under a per-name mutex. The in-memory store is by definition single-process, so
// a mutex is the whole of it - the interface exists for the Postgres store, where the same call is
// a cross-process advisory lock (see store.Store.WithLock).
func (m *Memory) WithLock(name string, fn func() error) error {
	mu, _ := m.locks.LoadOrStore(name, &sync.Mutex{})
	l := mu.(*sync.Mutex)
	l.Lock()
	defer l.Unlock()
	return fn()
}

func (m *Memory) CreateCluster(c *domain.Cluster) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.clusters[c.ID]; ok {
		return errors.New("cluster already exists")
	}
	m.clusters[c.ID] = clone(*c)
	return nil
}

func (m *Memory) GetCluster(id string) (*domain.Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clusters[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := clone(c)
	return &cp, nil
}

func (m *Memory) ListClusters() ([]*domain.Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.Cluster, 0, len(m.clusters))
	for _, c := range m.clusters {
		cp := clone(c)
		out = append(out, &cp)
	}
	return out, nil
}

func (m *Memory) ListClustersByOwner(ownerID string) ([]*domain.Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.Cluster, 0)
	for _, c := range m.clusters {
		if c.OwnerID != ownerID {
			continue
		}
		cp := clone(c)
		out = append(out, &cp)
	}
	return out, nil
}

func (m *Memory) UpdateCluster(c *domain.Cluster) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.clusters[c.ID]; !ok {
		return ErrNotFound
	}
	m.clusters[c.ID] = clone(*c)
	return nil
}

func (m *Memory) UpdateClusterUnlessSuperseded(c *domain.Cluster) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.clusters[c.ID]
	if !ok {
		return ErrNotFound
	}
	if existing.Generation != c.Generation {
		return ErrConflict // a newer desired generation landed (e.g. a delete); don't clobber it
	}
	m.clusters[c.ID] = clone(*c)
	return nil
}

func (m *Memory) ClustersNeedingWork() ([]*domain.Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*domain.Cluster
	for _, c := range m.clusters {
		if c.NeedsWork() {
			cp := clone(c)
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *Memory) ClustersDueCertRenewal(cutoff time.Time) ([]*domain.Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*domain.Cluster
	for _, c := range m.clusters {
		if c.CertRenewalDue(cutoff) {
			cp := clone(c)
			out = append(out, &cp)
		}
	}
	return out, nil
}

// ClustersDueEtcdMaintenance mirrors the Postgres scan's predicate: Ready, converged, and either
// never observed or last observed before cutoff.
func (m *Memory) ClustersDueEtcdMaintenance(cutoff time.Time) ([]*domain.Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*domain.Cluster
	for _, c := range m.clusters {
		if c.Phase != domain.PhaseReady || c.ObservedGeneration != c.Generation {
			continue
		}
		if c.Etcd == nil || c.Etcd.ObservedAt.Before(cutoff) {
			cp := clone(c)
			out = append(out, &cp)
		}
	}
	return out, nil
}

// ClustersDueEtcdSnapshot mirrors the Postgres scan's predicate: Ready, converged, and either never
// snapshotted or last snapshotted before cutoff.
func (m *Memory) ClustersDueEtcdSnapshot(cutoff time.Time) ([]*domain.Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*domain.Cluster
	for _, c := range m.clusters {
		if c.Phase != domain.PhaseReady || c.ObservedGeneration != c.Generation {
			continue
		}
		if c.EtcdSnapshotAt == nil || c.EtcdSnapshotAt.Before(cutoff) {
			cp := clone(c)
			out = append(out, &cp)
		}
	}
	return out, nil
}

// ClustersDueRepair mirrors the Postgres scan's predicate: Ready, converged, and either never
// observed or last observed before cutoff.
func (m *Memory) ClustersDueRepair(cutoff time.Time) ([]*domain.Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*domain.Cluster
	for _, c := range m.clusters {
		if c.Phase != domain.PhaseReady || c.ObservedGeneration != c.Generation {
			continue
		}
		if c.Repair == nil || c.Repair.ObservedAt.Before(cutoff) {
			cp := clone(c)
			out = append(out, &cp)
		}
	}
	return out, nil
}

// cloneUser deep-copies a user so the in-memory map and its callers never share the Memberships
// slice or the Quotas map - otherwise a caller filtering or mutating either would silently corrupt
// the store.
func cloneUser(u domain.User) domain.User {
	if u.Memberships != nil {
		u.Memberships = append([]domain.GroupMembership(nil), u.Memberships...)
	}
	u.Quotas = maps.Clone(u.Quotas)
	return u
}

func (m *Memory) CreateUser(u *domain.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[u.ID]; ok {
		return errors.New("user already exists")
	}
	for _, existing := range m.users {
		if existing.Username == u.Username {
			return ErrConflict
		}
	}
	m.users[u.ID] = cloneUser(*u)
	return nil
}

func (m *Memory) GetUser(id string) (*domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := cloneUser(u)
	return &cp, nil
}

func (m *Memory) GetUserByUsername(username string) (*domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.users {
		if u.Username == username {
			cp := cloneUser(u)
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListUsers() ([]*domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.User, 0, len(m.users))
	for _, u := range m.users {
		cp := cloneUser(u)
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) UpdateUser(u *domain.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[u.ID]; !ok {
		return ErrNotFound
	}
	for id, existing := range m.users { // username must stay unique
		if id != u.ID && existing.Username == u.Username {
			return ErrConflict
		}
	}
	m.users[u.ID] = cloneUser(*u)
	return nil
}

func (m *Memory) DeleteUser(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[id]; !ok {
		return ErrNotFound
	}
	delete(m.users, id)
	return nil
}

func (m *Memory) CreateGroup(g *domain.Group) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[g.ID]; ok {
		return errors.New("group already exists")
	}
	for _, existing := range m.groups {
		if existing.Name == g.Name {
			return ErrConflict
		}
	}
	m.groups[g.ID] = *g
	return nil
}

func (m *Memory) GetGroup(id string) (*domain.Group, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.groups[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := g
	return &cp, nil
}

// GetGroupBySource mirrors the Postgres store's partial unique index on (source, source_key): only
// directory-owned groups are addressable this way, since every local group shares the empty key.
func (m *Memory) GetGroupBySource(source, sourceKey string) (*domain.Group, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if source == domain.SourceLocal || source == "" || sourceKey == "" {
		return nil, ErrNotFound
	}
	for _, g := range m.groups {
		if g.Source == source && g.SourceKey == sourceKey {
			cp := g
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListGroups() ([]*domain.Group, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.Group, 0, len(m.groups))
	for _, g := range m.groups {
		cp := g
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) UpdateGroup(g *domain.Group) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[g.ID]; !ok {
		return ErrNotFound
	}
	for id, existing := range m.groups { // name must stay unique
		if id != g.ID && existing.Name == g.Name {
			return ErrConflict
		}
	}
	m.groups[g.ID] = *g
	return nil
}

func (m *Memory) DeleteGroup(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[id]; !ok {
		return ErrNotFound
	}
	delete(m.groups, id)
	return nil
}

// cloneCatalog deep-copies a custom catalog so the in-memory map and its callers never share the
// Addons slice - otherwise a caller mutating the slice would silently corrupt the store.
func cloneCatalog(cc domain.CustomCatalog) domain.CustomCatalog {
	cc.Addons = append([]domain.CustomAddon(nil), cc.Addons...)
	return cc
}

func (m *Memory) CreateCustomCatalog(cc *domain.CustomCatalog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.catalogs[cc.ID]; ok {
		return errors.New("custom catalog already exists")
	}
	for _, existing := range m.catalogs {
		if existing.OwnerID == cc.OwnerID && existing.Name == cc.Name {
			return ErrConflict
		}
	}
	m.catalogs[cc.ID] = cloneCatalog(*cc)
	return nil
}

func (m *Memory) GetCustomCatalog(id string) (*domain.CustomCatalog, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cc, ok := m.catalogs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := cloneCatalog(cc)
	return &cp, nil
}

func (m *Memory) ListCustomCatalogs() ([]*domain.CustomCatalog, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.CustomCatalog, 0, len(m.catalogs))
	for _, cc := range m.catalogs {
		cp := cloneCatalog(cc)
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) ListCustomCatalogsByOwner(ownerID string) ([]*domain.CustomCatalog, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.CustomCatalog, 0)
	for _, cc := range m.catalogs {
		if cc.OwnerID != ownerID {
			continue
		}
		cp := cloneCatalog(cc)
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) UpdateCustomCatalog(cc *domain.CustomCatalog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.catalogs[cc.ID]; !ok {
		return ErrNotFound
	}
	for id, existing := range m.catalogs { // (owner, name) must stay unique
		if id != cc.ID && existing.OwnerID == cc.OwnerID && existing.Name == cc.Name {
			return ErrConflict
		}
	}
	m.catalogs[cc.ID] = cloneCatalog(*cc)
	return nil
}

func (m *Memory) DeleteCustomCatalog(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.catalogs[id]; !ok {
		return ErrNotFound
	}
	delete(m.catalogs, id)
	return nil
}

func (m *Memory) SaveSecret(clusterID string, kind domain.SecretKind, ciphertext []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, len(ciphertext))
	copy(buf, ciphertext)
	m.secrets[clusterID+"/"+string(kind)] = buf
	return nil
}

func (m *Memory) GetSecret(clusterID string, kind domain.SecretKind) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.secrets[clusterID+"/"+string(kind)]
	if !ok {
		return nil, ErrNotFound
	}
	buf := make([]byte, len(v))
	copy(buf, v)
	return buf, nil
}

func (m *Memory) RecordOperation(op *domain.Operation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.operations[op.ClusterID] = append(m.operations[op.ClusterID], cloneOp(*op))
	return nil
}

func (m *Memory) ListOperations(clusterID string) ([]*domain.Operation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ops := m.operations[clusterID]
	out := make([]*domain.Operation, 0, len(ops))
	for i := range ops {
		cp := cloneOp(ops[i])
		out = append(out, &cp)
	}
	// Newest first (append order is oldest-first).
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

func (m *Memory) CompleteOperations(clusterID string, throughGeneration int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ops := m.operations[clusterID]
	for i := range ops {
		// Self-completing kinds (an open SSH session, any platform-initiated maintenance/repair) close
		// themselves via CompleteOperation - the generation sweep must not touch them (see SweepExempt).
		if ops[i].Status == domain.OpInProgress && !ops[i].Kind.SweepExempt() && ops[i].Generation <= throughGeneration {
			ops[i].Status = domain.OpCompleted
			t := at
			ops[i].FinishedAt = &t
		}
	}
	return nil
}

func (m *Memory) CompleteOperation(id, detail string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for cid := range m.operations {
		ops := m.operations[cid]
		for i := range ops {
			if ops[i].ID == id {
				ops[i].Status = domain.OpCompleted
				t := at
				ops[i].FinishedAt = &t
				if detail != "" {
					ops[i].Detail = detail
				}
				return nil
			}
		}
	}
	return ErrNotFound
}

func (m *Memory) SaveMetrics(snapshot *domain.MetricsSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metrics[snapshot.ClusterID] = cloneMetrics(*snapshot)
	return nil
}

func (m *Memory) GetMetrics(clusterID string) (*domain.MetricsSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.metrics[clusterID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := cloneMetrics(s)
	return &cp, nil
}

func cloneMetrics(s domain.MetricsSnapshot) domain.MetricsSnapshot {
	cp := s
	cp.Nodes = append([]domain.NodeMetrics(nil), s.Nodes...)
	return cp
}

func (m *Memory) SaveHealth(snapshot *domain.HealthSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.health[snapshot.ClusterID] = cloneHealth(*snapshot)
	return nil
}

func (m *Memory) GetHealth(clusterID string) (*domain.HealthSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.health[clusterID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := cloneHealth(s)
	return &cp, nil
}

func cloneHealth(s domain.HealthSnapshot) domain.HealthSnapshot {
	cp := s
	cp.Checks = append([]domain.HealthCheck(nil), s.Checks...)
	cp.Nodes = append([]domain.NodeHealth(nil), s.Nodes...)
	return cp
}

func (m *Memory) SaveEtcdSnapshot(snap *domain.EtcdSnapshot, sealed []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots[snap.ClusterID] = append(m.snapshots[snap.ClusterID], *snap)
	m.snapshotData[snap.ID] = append([]byte(nil), sealed...)
	return nil
}

// ListEtcdSnapshots returns metadata newest-first, matching the Postgres ORDER BY so callers that
// take the head (the restore path) get the same answer from either store.
func (m *Memory) ListEtcdSnapshots(clusterID string) ([]domain.EtcdSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]domain.EtcdSnapshot(nil), m.snapshots[clusterID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].TakenAt.After(out[j].TakenAt) })
	return out, nil
}

func (m *Memory) GetEtcdSnapshotPayload(id string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.snapshotData[id]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (m *Memory) DeleteEtcdSnapshot(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.snapshotData, id)
	for cid, snaps := range m.snapshots {
		for i, s := range snaps {
			if s.ID == id {
				m.snapshots[cid] = append(snaps[:i:i], snaps[i+1:]...)
				return nil
			}
		}
	}
	return nil // deleting an absent snapshot is a no-op, like every other idempotent teardown here
}

func cloneOp(op domain.Operation) domain.Operation {
	cp := op
	if op.FinishedAt != nil {
		t := *op.FinishedAt
		cp.FinishedAt = &t
	}
	return cp
}

// clone deep-copies a Cluster so callers can't mutate stored state by reference.
func clone(c domain.Cluster) domain.Cluster {
	cp := c
	cp.Nodes = append([]domain.Node(nil), c.Nodes...)
	cp.NodePools = append([]domain.NodePool(nil), c.NodePools...)
	cp.NodeDisks = append([]domain.NodeDisk(nil), c.NodeDisks...)
	cp.Addons = append([]domain.Addon(nil), c.Addons...)
	cp.StaticIPs = maps.Clone(c.StaticIPs) // nil-safe
	if c.DeletedAt != nil {
		t := *c.DeletedAt
		cp.DeletedAt = &t
	}
	if c.CertNotAfter != nil {
		t := *c.CertNotAfter
		cp.CertNotAfter = &t
	}
	if c.EtcdSnapshotAt != nil {
		t := *c.EtcdSnapshotAt
		cp.EtcdSnapshotAt = &t
	}
	if c.Etcd != nil {
		e := *c.Etcd
		e.Alarms = append([]string(nil), c.Etcd.Alarms...)
		if c.Etcd.DefraggedAt != nil {
			t := *c.Etcd.DefraggedAt
			e.DefraggedAt = &t
		}
		cp.Etcd = &e
	}
	// The repair state is a map of POINTERS, so a shallow copy would hand every caller of the
	// in-memory store an alias into the stored row - and the reconciler mutates these in place
	// (stamping UnhealthySince, incrementing Attempts). Real state changes would then appear
	// without an Update, which is exactly the class of bug the in-memory store exists to not have.
	if c.Repair != nil {
		r := *c.Repair
		if c.Repair.Nodes != nil {
			r.Nodes = make(map[string]*domain.NodeRepairState, len(c.Repair.Nodes))
			for vm, st := range c.Repair.Nodes {
				s := *st
				s.UnhealthySince = cloneTime(st.UnhealthySince)
				s.LastActionAt = cloneTime(st.LastActionAt)
				s.RepairedAt = cloneTime(st.RepairedAt)
				r.Nodes[vm] = &s
			}
		}
		cp.Repair = &r
	}
	return cp
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
