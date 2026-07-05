package reconcile

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// snapCfg drives the snapshot seam: it records how often a backup was taken and can be made to fail.
type snapCfg struct {
	config.Fake
	calls   int
	payload []byte
	err     error
}

func (c *snapCfg) SnapshotEtcd(_ context.Context, cl *domain.Cluster) (domain.EtcdSnapshot, []byte, error) {
	c.calls++
	if c.err != nil {
		return domain.EtcdSnapshot{}, nil, c.err
	}
	payload := c.payload
	if payload == nil {
		payload = []byte("etcd-keyspace-and-pki")
	}
	return domain.EtcdSnapshot{
		TakenAt: time.Now().UTC(), Revision: int64(1000 + c.calls), Hash: 42,
		K8sVersion: cl.K8sVersion, NodeName: "cp-0",
	}, payload, nil
}

func snapshotTestPolicy() domain.EtcdSnapshotPolicy {
	return domain.EtcdSnapshotPolicy{
		Enabled: true, Interval: 6 * time.Hour, Retain: 2, MaxRestoreAge: 24 * time.Hour,
	}
}

// TestSnapshotTakenSealedAndStored: the Ready tick promotes a never-snapshotted cluster, the phase
// stores a SEALED payload, and the cadence marker moves.
func TestSnapshotTakenSealedAndStored(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &snapCfg{payload: []byte("secret-keyspace")}
	r.Cfg = cfg
	r.SnapshotPolicy = snapshotTestPolicy()

	c := readyCluster(t, r, st, "sn1")
	if !c.EtcdSnapshotDue(r.SnapshotPolicy, time.Now()) {
		t.Fatal("a never-snapshotted Ready cluster should be due")
	}

	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.Phase != domain.PhaseSnapshottingEtcd {
		t.Fatalf("phase = %s, want SnapshottingEtcd", c.Phase)
	}
	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s after snapshotting, want Ready", c.Phase)
	}
	if cfg.calls != 1 {
		t.Fatalf("SnapshotEtcd called %d times, want 1", cfg.calls)
	}
	if c.EtcdSnapshotAt == nil {
		t.Fatal("cadence marker not stamped")
	}
	if c.EtcdSnapshotDue(r.SnapshotPolicy, time.Now()) {
		t.Fatal("still due immediately after a successful snapshot")
	}

	snaps, err := st.ListEtcdSnapshots(c.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("stored %d snapshots (%v), want 1", len(snaps), err)
	}
	// The payload must be SEALED at rest: it is the cluster's entire Secret set plus the CA key, so
	// finding the plaintext in the store would be the single worst bug this feature could have.
	sealed, err := st.GetEtcdSnapshotPayload(snaps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("secret-keyspace")) {
		t.Fatal("snapshot payload was stored unsealed")
	}
	opened, err := r.Secrets.Open(sealed)
	if err != nil {
		t.Fatalf("stored payload does not unseal: %v", err)
	}
	if !bytes.Equal(opened, []byte("secret-keyspace")) {
		t.Fatalf("unsealed payload = %q, want the original", opened)
	}
}

// TestSnapshotRecordsOperation: a scheduled backup is entered in the Operations history and closed
// with the etcd revision + sealed size it stored, so the Activity tab reads as a backup log without
// the operator digging through the finer-grained event timeline.
func TestSnapshotRecordsOperation(t *testing.T) {
	r, st := newTestReconciler(t)
	r.Cfg = &snapCfg{payload: []byte("secret-keyspace")}
	r.SnapshotPolicy = snapshotTestPolicy()

	c := readyCluster(t, r, st, "snop")
	if err := r.reconcileOne(context.Background(), c); err != nil { // Ready -> SnapshottingEtcd
		t.Fatal(err)
	}
	if err := r.reconcileOne(context.Background(), c); err != nil { // take it
		t.Fatal(err)
	}

	ops := opsOfKind(t, st, c.ID, domain.OpSnapshot)
	if len(ops) != 1 {
		t.Fatalf("recorded %d snapshot ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Status != domain.OpCompleted || op.FinishedAt == nil {
		t.Fatalf("snapshot op not completed: status=%s finished=%v", op.Status, op.FinishedAt)
	}
	if !strings.Contains(op.Detail, "revision") {
		t.Fatalf("detail = %q, want it to carry the etcd revision", op.Detail)
	}
	// A completed platform op must survive the generation sweep untouched - it is exempt (it carries
	// no generation), so a converged Ready tick must not reopen or re-stamp it.
	if err := st.CompleteOperations(c.ID, c.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	if again := opsOfKind(t, st, c.ID, domain.OpSnapshot); again[0].Status != domain.OpCompleted {
		t.Fatal("generation sweep disturbed a self-completing snapshot op")
	}
}

// TestFailedSnapshotDoesNotAdvanceTheCadence: stamping the marker on a failed backup would push the
// next attempt a full interval away, halving the coverage of exactly the cluster that can least
// afford it.
func TestFailedSnapshotDoesNotAdvanceTheCadence(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &snapCfg{err: errors.New("etcdctl: snapshot save failed")}
	r.Cfg = cfg
	r.SnapshotPolicy = snapshotTestPolicy()

	c := readyCluster(t, r, st, "sn2")
	if err := r.reconcileOne(context.Background(), c); err != nil { // Ready -> SnapshottingEtcd
		t.Fatal(err)
	}
	if err := r.reconcileOne(context.Background(), c); err == nil {
		t.Fatal("a failed snapshot did not fail the reconcile step")
	}
	if c.EtcdSnapshotAt != nil {
		t.Fatal("cadence marker advanced despite the snapshot failing")
	}
	if snaps, _ := st.ListEtcdSnapshots(c.ID); len(snaps) != 0 {
		t.Fatalf("stored %d snapshots after a failure", len(snaps))
	}
	// A failed backup is still recorded and closed (with its error) rather than left dangling - the
	// defer in reconcileSnapshot covers every early return.
	ops := opsOfKind(t, st, c.ID, domain.OpSnapshot)
	if len(ops) != 1 || ops[0].Status != domain.OpCompleted || !strings.HasPrefix(ops[0].Detail, "failed:") {
		t.Fatalf("failed snapshot op = %+v, want one completed op with a \"failed:\" detail", ops)
	}
}

// TestRetentionPrunesOldest, and never prunes to nothing.
func TestRetentionPrunesOldest(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &snapCfg{}
	r.Cfg = cfg
	r.SnapshotPolicy = snapshotTestPolicy() // Retain: 2

	c := readyCluster(t, r, st, "sn3")
	for i := range 4 {
		c.EtcdSnapshotAt = nil // force due again
		if err := r.reconcileOne(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileOne(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		// Snapshots minted in the same wall-clock second must still order; nudge time forward.
		if i < 3 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	snaps, err := st.ListEtcdSnapshots(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("retained %d snapshots, want 2", len(snaps))
	}
	// Newest first, and the survivors are the two most recent revisions (1003, 1004).
	if snaps[0].Revision != 1004 || snaps[1].Revision != 1003 {
		t.Fatalf("retained revisions %d and %d, want 1004 and 1003", snaps[0].Revision, snaps[1].Revision)
	}
}

// TestSnapshotRanksAboveDefrag pins the ordering that closes the gap the defrag feature left open:
// with one phase per invocation, a cluster due for both backs up first and defragments next tick,
// so a sole control plane's stop-the-world defrag is never the first thing that happens to it.
func TestSnapshotRanksAboveDefrag(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &snapCfg{}
	r.Cfg = cfg
	r.SnapshotPolicy = snapshotTestPolicy()
	r.EtcdPolicy = etcdTestPolicy()

	c := readyCluster(t, r, st, "sn4")
	// Due for both: never snapshotted, and etcd never observed.
	if !c.EtcdSnapshotDue(r.SnapshotPolicy, time.Now()) || !c.EtcdMaintenanceDue(r.EtcdPolicy, time.Now()) {
		t.Fatal("setup: expected the cluster to be due for both")
	}

	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.Phase != domain.PhaseSnapshottingEtcd {
		t.Fatalf("phase = %s, want SnapshottingEtcd - the backup must come first", c.Phase)
	}
}

// TestSnapshotDisabledIsInert.
func TestSnapshotDisabledIsInert(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &snapCfg{}
	r.Cfg = cfg // SnapshotPolicy left at its zero value

	c := readyCluster(t, r, st, "sn5")
	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.Phase != domain.PhaseReady || cfg.calls != 0 {
		t.Fatalf("disabled snapshots acted: phase=%s calls=%d", c.Phase, cfg.calls)
	}
}

// TestRestoreRefusesWithNoSnapshot: the recovery path must say so plainly rather than fail
// mysteriously - this is read by someone whose cluster is already down.
func TestRestoreRefusesWithNoSnapshot(t *testing.T) {
	r, st := newTestReconciler(t)
	r.SnapshotPolicy = snapshotTestPolicy()
	c := readyCluster(t, r, st, "sn6")

	err := r.restoreSnapshot(context.Background(), c, domain.Node{VMName: "sn6-cp-0"})
	if err == nil {
		t.Fatal("restored from an empty snapshot set")
	}
}
