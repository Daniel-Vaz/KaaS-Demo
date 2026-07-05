package domain

import (
	"fmt"
	"time"
)

// EtcdSnapshot is the metadata for ONE stored control-plane backup: an `etcdctl snapshot save` of
// the cluster's keyspace together with the cluster PKI and the kubelet's own state directory,
// sealed and held in the platform's database.
//
// Why the PKI rides along, and is not optional: a keyspace snapshot alone cannot rebuild a control
// plane. Without the ORIGINAL CA private key the restored API server serves a different CA, and
// every kubelet client cert, every per-user kubeconfig minted by kube.MintUserKubeconfig, and every
// ClusterRoleBinding the viewer_kubeconfig role keyed on a cert subject stops verifying. Restoring
// half the state is not a partial recovery, it is a differently-broken cluster.
//
// Which makes the second fact about this type the important one: A SNAPSHOT IS THE ENTIRE CLUSTER'S
// SECRETS IN PLAINTEXT, PLUS THE CA PRIVATE KEY. etcd stores Secret objects unencrypted at rest by
// default (this platform does not configure an EncryptionConfiguration), so the payload is strictly
// more sensitive than anything else the platform holds - including the admin kubeconfig, which is
// merely a credential TO the cluster rather than a copy OF it. Consequences, all deliberate:
//
//   - The payload is sealed with secrets.Box before it reaches the store and is never written to
//     disk unsealed outside the worker's own per-cluster artifacts dir.
//   - It is exposed through NO API surface. There is no download endpoint, no list-with-bytes, and
//     the portal never sees more than the metadata in this struct. A "download backup" button would
//     hand any cluster owner an offline copy of every Secret their cluster has ever held.
//   - Only the worker can produce or consume one - it is the only component holding both
//     KAAS_SECRET_KEY and a route to the cluster, exactly as with SecretKubeconfig.
//
// Production would put the payload in object storage under a KMS-backed key with its own retention
// and audit policy, rather than in the platform's own Postgres under an env-derived AES key.
type EtcdSnapshot struct {
	// ID is the snapshot's own identity, so a restore can name the one it used in an event.
	ID        string `json:"id"`
	ClusterID string `json:"cluster_id"`
	// TakenAt is when the snapshot was written, and the only ordering that matters: restore picks
	// the newest VALID one (see RestorableSnapshot).
	TakenAt time.Time `json:"taken_at"`
	// Revision is the etcd store revision the snapshot captured, read back from
	// `etcdutl snapshot status`. Reported for the portal and, more usefully, as the thing that makes
	// "how much would a restore roll back" answerable at all.
	Revision int64 `json:"revision"`
	// Hash is the snapshot file's own integrity hash, likewise from `snapshot status`. Stored because
	// it is what the verification step produced: a file that cannot be hashed is not a backup.
	Hash uint32 `json:"hash,omitempty"`
	// SizeBytes is the size of the SEALED payload - what the snapshot actually costs the platform's
	// database, which is the number an operator sizing retention needs.
	SizeBytes int64 `json:"size_bytes"`
	// K8sVersion is the Kubernetes version the cluster ran when the snapshot was taken. A restore
	// onto a node built from a different bundle's golden image would put back manifests for another
	// version, so this is recorded to make that mismatch visible rather than mysterious.
	K8sVersion string `json:"k8s_version,omitempty"`
	// NodeName is the control plane the snapshot was taken from.
	NodeName string `json:"node_name,omitempty"`
}

// Age is how far back a restore of this snapshot would roll the cluster.
func (s EtcdSnapshot) Age(now time.Time) time.Duration { return now.Sub(s.TakenAt) }

// MaxEtcdSnapshotBytes caps the sealed payload the platform is willing to put in a row. etcd's own
// backend quota is 8GiB on a tuned cluster, and a snapshot of one would be a multi-gigabyte
// parameter on a single INSERT - which does not fail gracefully, it exhausts the worker's memory and
// then Postgres's. A snapshot over this is refused and reported rather than attempted: an operator
// with a keyspace that big needs real object storage, which is the production answer anyway.
const MaxEtcdSnapshotBytes int64 = 512 * 1024 * 1024

// EtcdSnapshotPolicy decides WHEN a cluster is snapshotted and WHETHER a stored snapshot may be
// restored. Like EtcdDefragPolicy it is a plain value, so the whole decision is unit-testable
// without a cluster, an ansible run, or a clock.
type EtcdSnapshotPolicy struct {
	// Enabled turns the feature on. Off means no snapshots are taken and - because a restore can
	// only ever consume one - a dead sole control plane is not recoverable.
	Enabled bool
	// Interval is how often a Ready cluster is snapshotted. It is also the bound on how much a
	// restore can lose, which is the number to reason about when choosing it.
	Interval time.Duration
	// Retain is how many snapshots per cluster are kept. Bounded because retention multiplies by the
	// fleet: Retain x clusters x payload size all lives in one Postgres.
	Retain int
	// MaxRestoreAge is how stale a snapshot may be and still be restored automatically. Past it the
	// platform refuses and surfaces the cluster as unrecoverable-without-a-human, because putting
	// back a weeks-old keyspace is not obviously better than the outage it replaces - every object
	// created since would vanish, including ones other systems believe exist.
	MaxRestoreAge time.Duration
}

// SnapshotDue reports whether a cluster whose last snapshot was taken at last should be snapshotted
// now. A cluster that has never been snapshotted is always due - the one-time backfill for clusters
// predating this feature, the same signal CertNotAfter == nil and a NULL etcd_observed_at carry.
func (p EtcdSnapshotPolicy) SnapshotDue(last *time.Time, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	return last == nil || last.IsZero() || now.Sub(*last) >= p.Interval
}

// EtcdSnapshotDue reports whether the reconciler has snapshot work for this cluster now. Narrow in
// exactly the way CertRenewalDue and EtcdMaintenanceDue are: only a Ready, converged cluster
// qualifies, so a snapshot never races an in-flight transition - a mid-upgrade snapshot would
// capture a cluster that is half-way between two bundles, which is the one state nobody wants to
// restore to.
func (c *Cluster) EtcdSnapshotDue(p EtcdSnapshotPolicy, now time.Time) bool {
	if c.Phase != PhaseReady || c.ObservedGeneration != c.Generation {
		return false
	}
	return p.SnapshotDue(c.EtcdSnapshotAt, now)
}

// RestorableSnapshot picks the snapshot a recovery should use from a cluster's stored set, newest
// first, and explains itself either way - the "why not" being the half an operator staring at a
// dead cluster actually needs.
//
// Newest-first rather than a search for the best one: every snapshot here has already passed
// verification at take time (an unverifiable one is never stored - see config.Manager.SnapshotEtcd),
// so among valid candidates the newest is unambiguously the least lossy.
func RestorableSnapshot(snaps []EtcdSnapshot, p EtcdSnapshotPolicy, now time.Time) (EtcdSnapshot, string, bool) {
	if len(snaps) == 0 {
		return EtcdSnapshot{}, "no etcd snapshot has been stored for this cluster", false
	}
	newest := snaps[0]
	for _, s := range snaps[1:] {
		if s.TakenAt.After(newest.TakenAt) {
			newest = s
		}
	}
	if p.MaxRestoreAge > 0 && newest.Age(now) > p.MaxRestoreAge {
		return EtcdSnapshot{}, fmt.Sprintf("newest etcd snapshot is %s old, beyond the %s restore limit",
			HumanDuration(newest.Age(now)), HumanDuration(p.MaxRestoreAge)), false
	}
	return newest, fmt.Sprintf("restoring etcd snapshot taken %s ago (revision %d)",
		HumanDuration(newest.Age(now)), newest.Revision), true
}

// PruneEtcdSnapshots returns the snapshots to DELETE so that only the newest Retain survive. It
// returns the losers rather than the keepers because deleting is what the caller does, and because
// "keep the newest N" written as a filter is the classic way to accidentally delete everything when
// Retain is misconfigured to zero - here a zero Retain is clamped rather than obeyed.
func (p EtcdSnapshotPolicy) PruneEtcdSnapshots(snaps []EtcdSnapshot) []EtcdSnapshot {
	retain := p.Retain
	if retain < 1 {
		retain = 1 // never prune to nothing: a backup policy that deletes the last backup is not one
	}
	if len(snaps) <= retain {
		return nil
	}
	sorted := make([]EtcdSnapshot, len(snaps))
	copy(sorted, snaps)
	// Newest first; a simple insertion sort, since Retain keeps these slices to a handful of entries.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].TakenAt.After(sorted[j-1].TakenAt); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted[retain:]
}

// HumanDuration renders a duration the way an operator reads it in an event or a health summary -
// coarse and unitful ("3d", "5h", "12m") rather than Go's exact "74h13m52.7s".
func HumanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
