package reconcile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// reconcileSnapshot is PhaseSnapshottingEtcd: take a control-plane backup, seal it, store it, and
// prune the oldest beyond retention.
//
// The whole step is non-disruptive - `etcdctl snapshot save` streams a consistent view while etcd
// keeps serving - which is what makes it safe to run against every healthy cluster on a cadence, and
// what separates it from the quiescing backup the rolling-replacement path takes.
func (r *Reconciler) reconcileSnapshot(ctx context.Context, c *domain.Cluster) (err error) {
	// One action-history entry per backup attempt, opened here and closed exactly once below. The
	// detail is filled with the outcome - the revision and sealed size on success, the error on
	// failure - so the Operations history reads as a backup log without the operator hunting the
	// event timeline. Closed via defer so every early return (all of them errors) is covered.
	opID := r.recordAutoOp(c.ID, domain.OpSnapshot, "Scheduled control-plane backup", "")
	var detail string
	defer func() {
		if err != nil {
			detail = "failed: " + err.Error()
		}
		r.completeAutoOp(opID, detail)
	}()

	snap, payload, err := r.Cfg.SnapshotEtcd(ctx, c)
	if err != nil {
		return fmt.Errorf("snapshot etcd: %w", err)
	}
	// Sealed BEFORE it reaches the store, and never held anywhere else. The payload is the cluster's
	// entire keyspace - every Secret in plaintext - plus the CA private key, which makes it the most
	// sensitive thing the platform ever handles. Same Box the kubeconfigs use; the worker is the only
	// component with the key, and it is the only component that ever produces or consumes one.
	sealed, err := r.Secrets.Seal(payload)
	if err != nil {
		return fmt.Errorf("seal etcd snapshot: %w", err)
	}
	snap.ID = newSnapshotID()
	snap.ClusterID = c.ID
	// SizeBytes is the SEALED size - what the snapshot actually costs the database, which is the
	// number an operator sizing retention needs, not the size of the plaintext they never see.
	snap.SizeBytes = int64(len(sealed))
	if err := r.Store.SaveEtcdSnapshot(&snap, sealed); err != nil {
		return fmt.Errorf("store etcd snapshot: %w", err)
	}

	// Only now: the cadence marker moves once a snapshot genuinely exists. Stamping it before the
	// store call would let a failed write push the next attempt a full interval away, quietly
	// halving the backup coverage of a cluster whose snapshots are failing - the exact cluster that
	// can least afford it.
	taken := snap.TakenAt
	c.EtcdSnapshotAt = &taken
	r.emit(c.ID, "info", "ansible", fmt.Sprintf("control-plane backup stored - etcd revision %d, %s sealed",
		snap.Revision, domain.HumanBytes(snap.SizeBytes)))
	detail = fmt.Sprintf("etcd revision %d · %s sealed", snap.Revision, domain.HumanBytes(snap.SizeBytes))

	r.pruneSnapshots(c)
	return nil
}

// pruneSnapshots drops the snapshots beyond the retention count, oldest first.
//
// Best-effort, and deliberately not an error: retention is housekeeping, and failing the reconcile
// step over it would put a cluster whose backups are working into a retry loop. The next snapshot
// prunes again. Note the asymmetry with the snapshot itself, which IS an error - one is about having
// a backup, the other about not having too many.
func (r *Reconciler) pruneSnapshots(c *domain.Cluster) {
	snaps, err := r.Store.ListEtcdSnapshots(c.ID)
	if err != nil {
		r.Log.Warn("etcd snapshot retention: list", "cluster", c.Name, "err", err)
		return
	}
	for _, old := range r.SnapshotPolicy.PruneEtcdSnapshots(snaps) {
		if err := r.Store.DeleteEtcdSnapshot(old.ID); err != nil {
			r.Log.Warn("etcd snapshot retention: delete", "cluster", c.Name, "snapshot", old.ID, "err", err)
			return
		}
		r.Log.Debug("pruned etcd snapshot", "cluster", c.Name, "snapshot", old.ID, "taken", old.TakenAt)
	}
}

// restoreSnapshot rebuilds a sole control plane from the newest restorable stored snapshot. It is
// the last rung of automatic repair and the platform's only lossy operation - see
// domain.RepairPolicy.destructiveAction for when it is reached, and never from anywhere else.
func (r *Reconciler) restoreSnapshot(ctx context.Context, c *domain.Cluster, node domain.Node) error {
	snaps, err := r.Store.ListEtcdSnapshots(c.ID)
	if err != nil {
		return fmt.Errorf("list etcd snapshots: %w", err)
	}
	snap, reason, ok := domain.RestorableSnapshot(snaps, r.SnapshotPolicy, time.Now())
	if !ok {
		// Not a transient failure to retry: there is nothing to restore FROM, and no amount of
		// retrying creates one. Surfacing it plainly is the whole value here - an operator staring
		// at a dead cluster needs to know it is not coming back on its own.
		return fmt.Errorf("cannot restore %s: %s", c.Name, reason)
	}
	sealed, err := r.Store.GetEtcdSnapshotPayload(snap.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("etcd snapshot %s has metadata but no payload", snap.ID)
		}
		return fmt.Errorf("read etcd snapshot payload: %w", err)
	}
	payload, err := r.Secrets.Open(sealed)
	if err != nil {
		return fmt.Errorf("unseal etcd snapshot: %w", err)
	}
	r.emit(c.ID, "warn", "reconciler", reason+" - every object written since is lost")

	if err := r.Cfg.RestoreEtcdSnapshot(ctx, c, node, payload); err != nil {
		return fmt.Errorf("restore etcd snapshot: %w", err)
	}
	// The restored control plane's certificates came out of the snapshot, so their expiry is whatever
	// it was when the backup was taken - not what the platform last observed. Drop the observation so
	// the next Ready tick re-reads it and renews if the restore put back certs already near expiry.
	c.CertNotAfter = nil
	// Same for the etcd status: the backend file is a freshly materialised one with different size
	// and fragmentation than anything previously observed.
	c.Etcd = nil
	r.emit(c.ID, "info", "ansible", fmt.Sprintf("control plane %s rebuilt from the backup taken %s",
		node.VMName, snap.TakenAt.Format(time.RFC3339)))
	return nil
}

// newSnapshotID mints a snapshot's identity. Time-ordered so the ids sort the way the snapshots do,
// which makes a listing readable without joining on taken_at, and salted so two workers snapshotting
// the same cluster in the same second cannot collide on the primary key.
func newSnapshotID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("snap-%d-%s", time.Now().UTC().Unix(), hex.EncodeToString(b))
}
