package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

const mib = 1024 * 1024

// etcdCfg is a config.Manager that lets a test drive automatic etcd maintenance: it controls the
// status EtcdStatus observes and counts both seam calls. Every other method comes from config.Fake -
// including, importantly, NOT its EtcdStatus, whose synthetic drift would fight the test.
type etcdCfg struct {
	config.Fake
	status       domain.EtcdStatus // what EtcdStatus returns
	statusCalls  int
	defragCalls  int
	defragRatios []float64
}

func (c *etcdCfg) EtcdStatus(context.Context, *domain.Cluster) (domain.EtcdStatus, error) {
	c.statusCalls++
	st := c.status
	st.ObservedAt = time.Now()
	return st, nil
}

func (c *etcdCfg) DefragEtcd(_ context.Context, _ *domain.Cluster, minRatio float64) (domain.EtcdStatus, error) {
	c.defragCalls++
	c.defragRatios = append(c.defragRatios, minRatio)
	now := time.Now()
	// A defragmented store: the file now matches what it holds.
	return domain.EtcdStatus{
		DBBytes:      c.status.DBInUseBytes,
		DBInUseBytes: c.status.DBInUseBytes,
		QuotaBytes:   c.status.QuotaBytes,
		Members:      c.status.Members,
		ObservedAt:   now,
		DefraggedAt:  &now,
	}, nil
}

func etcdTestPolicy() domain.EtcdDefragPolicy {
	return domain.EtcdDefragPolicy{
		Enabled:         true,
		ObserveInterval: 6 * time.Hour,
		MinRatio:        0.45,
		MinBytes:        100 * mib,
		MinInterval:     24 * time.Hour,
	}
}

// fragmented is an observed store of total size db at ratio fragmentation, seen by all members.
func fragmented(db int64, ratio float64, members int) domain.EtcdStatus {
	return domain.EtcdStatus{
		DBBytes:      db,
		DBInUseBytes: int64(float64(db) * (1 - ratio)),
		QuotaBytes:   8 * 1024 * mib,
		Members:      members,
	}
}

// TestEtcdDefragWhenFragmented walks the whole path: a Ready cluster is surfaced as etcd-due,
// observed, promoted to DefragmentingEtcd, defragmented, and left Ready with the reclaimed size and
// the defrag timestamp stamped - after which it is no longer due.
func TestEtcdDefragWhenFragmented(t *testing.T) {
	r, st := newTestReconciler(t)
	r.EtcdPolicy = etcdTestPolicy()
	cfg := &etcdCfg{status: fragmented(500*mib, 0.60, 1)}
	r.Cfg = cfg

	readyCluster(t, r, st, "ed1")

	// Surfaced in both the dedicated query and the unioned work set (never observed = due).
	due, err := r.Store.ClustersDueEtcdMaintenance(time.Now().Add(-r.EtcdPolicy.ObserveInterval))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != "ed1" {
		t.Fatalf("ClustersDueEtcdMaintenance = %v, want [ed1]", due)
	}
	if work, _ := r.clustersNeedingWork(); len(work) != 1 || work[0].ID != "ed1" {
		t.Fatalf("clustersNeedingWork = %v, want [ed1]", work)
	}

	// Tick 1: observe -> DefragmentingEtcd. Nothing disruptive has happened yet.
	step(t, r, st, "ed1")
	got, _ := st.GetCluster("ed1")
	if got.Phase != domain.PhaseDefragmentingEtcd {
		t.Fatalf("after tick 1: phase = %s, want DefragmentingEtcd", got.Phase)
	}
	if cfg.statusCalls != 1 {
		t.Fatalf("EtcdStatus calls = %d, want 1", cfg.statusCalls)
	}
	if cfg.defragCalls != 0 {
		t.Fatalf("DefragEtcd called before the defrag phase (%d)", cfg.defragCalls)
	}
	if got.Etcd == nil || got.Etcd.DBBytes != 500*mib {
		t.Fatalf("observed status not stamped: %+v", got.Etcd)
	}

	// Tick 2: DefragmentingEtcd -> Ready, store reclaimed.
	step(t, r, st, "ed1")
	got, _ = st.GetCluster("ed1")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("after tick 2: phase = %s, want Ready", got.Phase)
	}
	if cfg.defragCalls != 1 {
		t.Fatalf("DefragEtcd calls = %d, want 1", cfg.defragCalls)
	}
	// The policy's threshold is forwarded so the playbook can skip members an earlier, killed attempt
	// already did - the property that makes a retried run resume instead of re-bouncing every member.
	if len(cfg.defragRatios) != 1 || cfg.defragRatios[0] != 0.45 {
		t.Fatalf("DefragEtcd minRatio = %v, want [0.45]", cfg.defragRatios)
	}
	if got.Etcd == nil || got.Etcd.FragmentationRatio() != 0 {
		t.Fatalf("post-defrag status not stamped: %+v", got.Etcd)
	}
	if got.Etcd.DefraggedAt == nil {
		t.Fatal("DefraggedAt not stamped")
	}
	if again, _ := r.Store.ClustersDueEtcdMaintenance(time.Now().Add(-r.EtcdPolicy.ObserveInterval)); len(again) != 0 {
		t.Fatalf("still due immediately after observation: %v", again)
	}
}

// TestEtcdObservationBelowThreshold: the common case. Observe, stamp, stay Ready - the fleet-wide
// picture exists whether or not defragmentation ever fires.
func TestEtcdObservationBelowThreshold(t *testing.T) {
	r, st := newTestReconciler(t)
	r.EtcdPolicy = etcdTestPolicy()
	cfg := &etcdCfg{status: fragmented(500*mib, 0.10, 1)}
	r.Cfg = cfg

	readyCluster(t, r, st, "eo1")
	step(t, r, st, "eo1")

	got, _ := st.GetCluster("eo1")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready (not due)", got.Phase)
	}
	if cfg.statusCalls != 1 || cfg.defragCalls != 0 {
		t.Fatalf("status=%d defrag=%d, want 1/0", cfg.statusCalls, cfg.defragCalls)
	}
	if got.Etcd == nil {
		t.Fatal("status not stamped")
	}
}

// TestEtcdDefragRefusedWithMemberDown is the safety guard end to end: the observation reports fewer
// members than the cluster has control planes, so the cluster stays Ready and untouched.
func TestEtcdDefragRefusedWithMemberDown(t *testing.T) {
	r, st := newTestReconciler(t)
	r.EtcdPolicy = etcdTestPolicy()
	cfg := &etcdCfg{status: fragmented(500*mib, 0.90, 2)} // 2 of 3 answered
	r.Cfg = cfg

	c := &domain.Cluster{
		ID: "em1", Name: "em1", Size: "small", K8sVersion: "1.36.2", ControlPlanes: 3,
		Phase: domain.PhaseReady, Generation: 1, ObservedGeneration: 1,
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	step(t, r, st, "em1")

	got, _ := st.GetCluster("em1")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready (a member is unreachable)", got.Phase)
	}
	if cfg.defragCalls != 0 {
		t.Fatalf("defragmented with a member down (%d calls)", cfg.defragCalls)
	}
}

// TestCertRenewalPrecedesEtcdMaintenance: both are periodic maintenance on a Ready cluster, but an
// expiring certificate is a deadline and a defrag is discretionary, so certificates win the tick.
func TestCertRenewalPrecedesEtcdMaintenance(t *testing.T) {
	r, st := newTestReconciler(t)
	r.CertRenewWindow = 30 * 24 * time.Hour
	r.EtcdPolicy = etcdTestPolicy()
	cfg := &etcdCfg{status: fragmented(500*mib, 0.90, 1)}
	r.Cfg = cfg

	soon := time.Now().Add(3 * 24 * time.Hour)
	c := &domain.Cluster{
		ID: "ec1", Name: "ec1", Size: "small", K8sVersion: "1.36.2", ControlPlanes: 1,
		Phase: domain.PhaseReady, Generation: 1, ObservedGeneration: 1, CertNotAfter: &soon,
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	step(t, r, st, "ec1")

	if got, _ := st.GetCluster("ec1"); got.Phase != domain.PhaseRenewingCerts {
		t.Fatalf("phase = %s, want RenewingCerts (certificates outrank etcd maintenance)", got.Phase)
	}
	if cfg.statusCalls != 0 || cfg.defragCalls != 0 {
		t.Fatalf("etcd seams called while a renewal was due: status=%d defrag=%d", cfg.statusCalls, cfg.defragCalls)
	}
}

// TestEtcdMaintenanceDisabled: the zero policy turns the feature off entirely - a badly fragmented
// Ready cluster is neither surfaced as work nor observed.
func TestEtcdMaintenanceDisabled(t *testing.T) {
	r, st := newTestReconciler(t) // EtcdPolicy defaults to the zero value (disabled)
	cfg := &etcdCfg{status: fragmented(8*1024*mib, 0.90, 1)}
	r.Cfg = cfg

	readyCluster(t, r, st, "ex1")
	if work, _ := r.clustersNeedingWork(); len(work) != 0 {
		t.Fatalf("disabled: clustersNeedingWork = %v, want none", work)
	}
	ready, _ := st.GetCluster("ex1")
	if err := r.reconcileOne(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	if cfg.statusCalls != 0 || cfg.defragCalls != 0 {
		t.Fatalf("disabled: etcd seams called: status=%d defrag=%d", cfg.statusCalls, cfg.defragCalls)
	}
	if got, _ := st.GetCluster("ex1"); got.Phase != domain.PhaseReady {
		t.Fatalf("disabled: phase drifted to %s", got.Phase)
	}
}

// TestEtcdDefragHysteresisAcrossTicks: after a defragmentation the store is stamped, and a cluster
// whose keyspace is genuinely large must not immediately defragment again on the next observation.
func TestEtcdDefragHysteresisAcrossTicks(t *testing.T) {
	r, st := newTestReconciler(t)
	r.EtcdPolicy = etcdTestPolicy()
	// The observation keeps reporting a heavily fragmented store even after a defrag - the pathological
	// case the minimum interval exists for.
	cfg := &etcdCfg{status: fragmented(500*mib, 0.90, 1)}
	r.Cfg = cfg

	readyCluster(t, r, st, "eh1")
	step(t, r, st, "eh1") // observe -> DefragmentingEtcd
	step(t, r, st, "eh1") // defrag -> Ready
	if cfg.defragCalls != 1 {
		t.Fatalf("DefragEtcd calls = %d, want 1", cfg.defragCalls)
	}

	// Force another observation and confirm the hysteresis floor holds.
	got, _ := st.GetCluster("eh1")
	got.Etcd.ObservedAt = time.Now().Add(-24 * time.Hour)
	if err := st.UpdateCluster(got); err != nil {
		t.Fatal(err)
	}
	step(t, r, st, "eh1")

	got, _ = st.GetCluster("eh1")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready (within the 24h minimum interval)", got.Phase)
	}
	if cfg.defragCalls != 1 {
		t.Fatalf("DefragEtcd calls = %d, want 1 - the minimum interval did not hold", cfg.defragCalls)
	}
}
