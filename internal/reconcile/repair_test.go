package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// opsOfKind returns a cluster's recorded operations of one kind - the assertion hook for the
// action-history entries the reconciler writes for its own automated maintenance and repair.
func opsOfKind(t *testing.T, st store.Store, clusterID string, kind domain.OperationKind) []*domain.Operation {
	t.Helper()
	all, err := st.ListOperations(clusterID)
	if err != nil {
		t.Fatal(err)
	}
	var out []*domain.Operation
	for _, o := range all {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	return out
}

// repairCfg counts the repair-shaped seam calls, so a test can assert WHICH rung ran rather than
// only that the phase advanced.
type repairCfg struct {
	config.Fake
	kubeletRestarts []string
	joinWorkers     int
	restores        []string
	restartErr      error
}

func (c *repairCfg) RestartKubelet(_ context.Context, _ *domain.Cluster, n domain.Node) error {
	c.kubeletRestarts = append(c.kubeletRestarts, n.VMName)
	return c.restartErr
}

func (c *repairCfg) JoinWorkers(context.Context, *domain.Cluster) error {
	c.joinWorkers++
	return nil
}

func (c *repairCfg) RestoreEtcdSnapshot(_ context.Context, _ *domain.Cluster, n domain.Node, _ []byte) error {
	c.restores = append(c.restores, n.VMName)
	return nil
}

func reconcileRepairPolicy() domain.RepairPolicy {
	return domain.RepairPolicy{
		Enabled:              true,
		Replace:              true,
		Restore:              true,
		ObserveInterval:      2 * time.Minute,
		HealthMaxAge:         time.Minute,
		NotReadyGrace:        10 * time.Minute,
		NodeStartupGrace:     20 * time.Minute,
		ReplaceAfter:         30 * time.Minute,
		MaxUnhealthyFraction: 0.5,
		MaxUnhealthyClusters: 0.5,
		MaxAttempts:          3,
		Backoff:              30 * time.Minute,
	}
}

// readyClusterWithWorkers converges a fresh cluster to Ready and returns it.
func readyClusterWithWorkers(t *testing.T, r *Reconciler, st store.Store, id string, workers int) *domain.Cluster {
	t.Helper()
	c := &domain.Cluster{
		ID: id, Name: id, K8sVersion: "1.36.2", Size: "small",
		NodePools: pool(workers), CNI: "cilium", Phase: domain.PhasePending, Generation: 1,
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, id)
	got, err := st.GetCluster(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// saveHealth stores a snapshot marking the named nodes NotReady and everything else Ready.
func saveHealth(t *testing.T, st store.Store, c *domain.Cluster, at time.Time, notReady ...string) {
	t.Helper()
	bad := map[string]bool{}
	for _, n := range notReady {
		bad[n] = true
	}
	snap := &domain.HealthSnapshot{
		ClusterID:   c.ID,
		CollectedAt: at,
		Checks:      []domain.HealthCheck{{ID: "api-server", Status: domain.HealthHealthy}},
	}
	for _, n := range c.Nodes {
		snap.Nodes = append(snap.Nodes, domain.NodeHealth{NodeName: n.VMName, Ready: !bad[n.VMName]})
	}
	if err := st.SaveHealth(snap); err != nil {
		t.Fatal(err)
	}
}

// TestRepairObservesAndStampsFaultWithoutActing: the first observation of a NotReady node records
// it and does nothing, because the grace period has not elapsed. The whole feature turns on this
// state existing - a health snapshot says a node is NotReady, only the stamped repair state says
// for how long.
func TestRepairObservesAndStampsFaultWithoutActing(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &repairCfg{}
	r.Cfg = cfg
	r.RepairPolicy = reconcileRepairPolicy()

	c := readyClusterWithWorkers(t, r, st, "rp1", 2)
	worker := workerName(t, c)
	saveHealth(t, st, c, time.Now(), worker)

	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready (the grace period should hold)", c.Phase)
	}
	st8 := c.RepairState().Nodes[worker]
	if st8 == nil || st8.Fault != domain.FaultNotReady || st8.UnhealthySince == nil {
		t.Fatalf("fault not stamped: %+v", st8)
	}
	if len(cfg.kubeletRestarts) != 0 {
		t.Fatalf("repaired %v inside the grace period", cfg.kubeletRestarts)
	}
}

// TestRepairRestartsKubeletAfterGrace walks the full two-phase shape: the Ready tick decides and
// promotes into PhaseRepairing, and the next invocation executes the rung.
func TestRepairRestartsKubeletAfterGrace(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &repairCfg{}
	r.Cfg = cfg
	r.RepairPolicy = reconcileRepairPolicy()

	c := readyClusterWithWorkers(t, r, st, "rp2", 2)
	worker := workerName(t, c)

	// Pre-stamp a fault that has already outlived the grace, the way successive observations would.
	since := time.Now().Add(-15 * time.Minute)
	c.RepairState().Nodes = map[string]*domain.NodeRepairState{
		worker: {Fault: domain.FaultNotReady, UnhealthySince: &since},
	}
	saveHealth(t, st, c, time.Now(), worker)

	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.Phase != domain.PhaseRepairing {
		t.Fatalf("phase = %s, want Repairing", c.Phase)
	}
	rs := c.RepairState()
	if rs.Target != worker || rs.Action != domain.ActionRestartKubelet {
		t.Fatalf("plan = %s on %s, want restart-kubelet on %s", rs.Action, rs.Target, worker)
	}
	// The attempt is stamped BEFORE the work, so a crash here still counts against the give-up limit.
	if rs.Nodes[worker].Attempts != 1 {
		t.Fatalf("attempts = %d before the repair ran, want 1", rs.Nodes[worker].Attempts)
	}

	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(cfg.kubeletRestarts) != 1 || cfg.kubeletRestarts[0] != worker {
		t.Fatalf("kubelet restarts = %v, want [%s]", cfg.kubeletRestarts, worker)
	}
	if c.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s after the repair, want Ready", c.Phase)
	}
	if rs.InFlight() {
		t.Fatal("plan still in flight after execution")
	}
	// And the fault is NOT cleared by the action returning: whether it worked is decided by the next
	// observation. Otherwise a kubelet restart that changes nothing would look like a success and the
	// ladder would never escalate.
	if rs.Nodes[worker].Fault != domain.FaultNotReady {
		t.Fatal("executing a repair cleared the fault without re-observing")
	}
}

// TestFailedRepairDoesNotFailTheReconcile: a failed repair is an ordinary outcome of repairing a
// broken thing. Returning it would hand the job to River's backoff, which would re-run the same rung
// outside the policy that decided it was due - the ladder has its own backoff and give-up counter.
func TestFailedRepairDoesNotFailTheReconcile(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &repairCfg{restartErr: context.DeadlineExceeded}
	r.Cfg = cfg
	r.RepairPolicy = reconcileRepairPolicy()

	c := readyClusterWithWorkers(t, r, st, "rp3", 1)
	worker := workerName(t, c)
	since := time.Now().Add(-15 * time.Minute)
	c.RepairState().Nodes = map[string]*domain.NodeRepairState{
		worker: {Fault: domain.FaultNotReady, UnhealthySince: &since},
	}
	saveHealth(t, st, c, time.Now(), worker)

	if err := r.reconcileOne(context.Background(), c); err != nil { // decide
		t.Fatal(err)
	}
	if err := r.reconcileOne(context.Background(), c); err != nil { // execute, and fail
		t.Fatalf("a failed repair failed the reconcile step: %v", err)
	}
	if c.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready", c.Phase)
	}
	if c.RepairState().Nodes[worker].Attempts != 1 {
		t.Fatal("a failed repair did not count as an attempt")
	}
	// The failed rung is still entered in the action history, closed with its error rather than left
	// dangling in_progress - a failed automated action is exactly what an operator needs to see.
	ops := opsOfKind(t, st, c.ID, domain.OpRepair)
	if len(ops) != 1 {
		t.Fatalf("recorded %d repair ops, want 1", len(ops))
	}
	if ops[0].Status != domain.OpCompleted {
		t.Fatalf("failed repair op left %s, want completed", ops[0].Status)
	}
	if !strings.HasPrefix(ops[0].Detail, "failed:") {
		t.Fatalf("failed repair op detail = %q, want it to start with \"failed:\"", ops[0].Detail)
	}
}

// TestRepairRecordsOperationInHistory: every automated repair rung is entered in the cluster's
// Operations history (the Activity tab) - opened when the rung RUNS (not when it is planned) and
// closed when it returns. This is the platform-initiated counterpart of a user's scale/upgrade op,
// and it carries no actor because the platform, not a user, is acting.
func TestRepairRecordsOperationInHistory(t *testing.T) {
	r, st := newTestReconciler(t)
	r.Cfg = &repairCfg{}
	r.RepairPolicy = reconcileRepairPolicy()

	c := readyClusterWithWorkers(t, r, st, "rpop", 2)
	worker := workerName(t, c)
	since := time.Now().Add(-15 * time.Minute)
	c.RepairState().Nodes = map[string]*domain.NodeRepairState{
		worker: {Fault: domain.FaultNotReady, UnhealthySince: &since},
	}
	saveHealth(t, st, c, time.Now(), worker)

	if err := r.reconcileOne(context.Background(), c); err != nil { // decide
		t.Fatal(err)
	}
	// Nothing recorded yet: the rung appears in history when it executes, not when it is chosen - the
	// same ordering as the attempt counter, but for the operator-facing log rather than the policy.
	if ops := opsOfKind(t, st, c.ID, domain.OpRepair); len(ops) != 0 {
		t.Fatalf("recorded %d repair ops at decision time, want 0", len(ops))
	}
	if err := r.reconcileOne(context.Background(), c); err != nil { // execute
		t.Fatal(err)
	}

	ops := opsOfKind(t, st, c.ID, domain.OpRepair)
	if len(ops) != 1 {
		t.Fatalf("recorded %d repair ops, want 1", len(ops))
	}
	op := ops[0]
	if op.Status != domain.OpCompleted || op.FinishedAt == nil {
		t.Fatalf("repair op not completed: status=%s finished=%v", op.Status, op.FinishedAt)
	}
	if !strings.Contains(op.Summary, worker) || !strings.Contains(op.Summary, "restart kubelet") {
		t.Fatalf("summary = %q, want it to name restart-kubelet on %s", op.Summary, worker)
	}
	if !strings.Contains(op.Detail, string(domain.FaultNotReady)) {
		t.Fatalf("detail = %q, want it to carry the fault", op.Detail)
	}
	if op.ActorID != "" || op.ActorUsername != "" {
		t.Fatalf("platform op has an actor (%q/%q), want none", op.ActorID, op.ActorUsername)
	}
	if op.Generation != 0 {
		t.Fatalf("platform op has generation %d, want 0 (no desired-state change)", op.Generation)
	}
}

// TestRepairStandsDownWhenApiServerIsUnreachable is the guard test at the reconcile level: a
// snapshot in which everything is NotReady because the API server is down must produce no action.
func TestRepairStandsDownWhenApiServerIsUnreachable(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &repairCfg{}
	r.Cfg = cfg
	r.RepairPolicy = reconcileRepairPolicy()

	c := readyClusterWithWorkers(t, r, st, "rp4", 2)
	snap := &domain.HealthSnapshot{
		ClusterID:   c.ID,
		CollectedAt: time.Now(),
		Status:      domain.HealthUnhealthy,
		Checks:      []domain.HealthCheck{{ID: "api-server", Status: domain.HealthUnhealthy}},
	}
	for _, n := range c.Nodes {
		snap.Nodes = append(snap.Nodes, domain.NodeHealth{NodeName: n.VMName, Ready: false})
	}
	if err := st.SaveHealth(snap); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready - an unobservable cluster must not be repaired", c.Phase)
	}
	if len(cfg.kubeletRestarts) != 0 {
		t.Fatalf("repaired %v on an unobservable cluster", cfg.kubeletRestarts)
	}
	if n := c.RepairState().Unhealthy(); n != 0 {
		t.Fatalf("recorded %d faults from an unobservable cluster", n)
	}
}

// TestRepairDisabledIsInert.
func TestRepairDisabledIsInert(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &repairCfg{}
	r.Cfg = cfg // RepairPolicy left at its zero value: disabled

	c := readyClusterWithWorkers(t, r, st, "rp5", 1)
	saveHealth(t, st, c, time.Now(), workerName(t, c))

	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.Phase != domain.PhaseReady || len(cfg.kubeletRestarts) != 0 {
		t.Fatalf("disabled repair acted: phase=%s restarts=%v", c.Phase, cfg.kubeletRestarts)
	}
	if c.Repair != nil {
		t.Fatal("disabled repair stamped state")
	}
}

// TestUpgradeOutranksRepair: during an upgrade nodes are drained and rebuilt on purpose, and every
// one of them looks exactly like the faults repair chases.
func TestUpgradeOutranksRepair(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &repairCfg{}
	r.Cfg = cfg
	r.RepairPolicy = reconcileRepairPolicy()

	c := readyClusterWithWorkers(t, r, st, "rp6", 1)
	worker := workerName(t, c)
	since := time.Now().Add(-time.Hour)
	c.RepairState().Nodes = map[string]*domain.NodeRepairState{
		worker: {Fault: domain.FaultNotReady, UnhealthySince: &since},
	}
	saveHealth(t, st, c, time.Now(), worker)
	// A pending generation change is enough: RepairDue requires Ready AND converged.
	c.Generation++

	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if c.Phase == domain.PhaseRepairing {
		t.Fatal("repair pre-empted a pending desired-state change")
	}
	if len(cfg.kubeletRestarts) != 0 {
		t.Fatalf("repaired %v while a change was pending", cfg.kubeletRestarts)
	}
}

// workerName returns the cluster's first worker VM name.
func workerName(t *testing.T, c *domain.Cluster) string {
	t.Helper()
	for _, n := range c.Nodes {
		if n.Role == domain.RoleWorker {
			return n.VMName
		}
	}
	t.Fatal("cluster has no workers")
	return ""
}

// TestRepairEscalatesToNodeReplacement walks the last rung a WORKER can reach: the cheap repair was
// tried, the fault outlived ReplaceAfter, and the node is rebuilt onto the SAME image it was already
// running - which is the whole reason a plain converge cannot express this and the provisioner has
// to be told explicitly.
func TestRepairEscalatesToNodeReplacement(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &repairCfg{}
	r.Cfg = cfg
	r.RepairPolicy = reconcileRepairPolicy()

	c := readyClusterWithWorkers(t, r, st, "rp7", 2)
	worker := workerName(t, c)
	imageBefore := nodeByName(c.Nodes, worker).Image
	ipBefore := nodeByName(c.Nodes, worker).IP

	// A fault that has already outlived both the grace and ReplaceAfter, with the cheap rung spent
	// and its backoff elapsed.
	since := time.Now().Add(-2 * time.Hour)
	acted := time.Now().Add(-90 * time.Minute)
	c.RepairState().Nodes = map[string]*domain.NodeRepairState{
		worker: {
			Fault: domain.FaultNotReady, UnhealthySince: &since, Attempts: 1,
			LastAction: domain.ActionRestartKubelet, LastActionAt: &acted,
		},
	}
	saveHealth(t, st, c, time.Now(), worker)

	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := c.RepairState().Action; got != domain.ActionReplace {
		t.Fatalf("action = %s, want replace", got)
	}
	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	// The node came back, on the same image (a repair is like-for-like, never a silent OS change)
	// and at the same address (the stable-IP-on-recreate contract every rejoin depends on).
	after := nodeByName(c.Nodes, worker)
	if after.VMName != worker {
		t.Fatalf("node %s missing after replacement", worker)
	}
	if after.Image != imageBefore {
		t.Fatalf("image changed during a repair: %q -> %q", imageBefore, after.Image)
	}
	if after.IP != ipBefore {
		t.Fatalf("address changed during a repair: %q -> %q", ipBefore, after.IP)
	}
	// A replaced worker is drained out and joined back via the ordinary idempotent paths.
	if cfg.joinWorkers == 0 {
		t.Fatal("replaced worker was never rejoined")
	}
	if c.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready", c.Phase)
	}
}

// TestDiskBearingNodeIsRebuiltNotRefused pins the invariant the uniform disk model establishes: a
// node carrying extra disks is REBUILT by the replace rung, not refused. Every backend now keeps a
// node's extra disks in resources independent of its VM (a libvirt_volume, a vsphere_virtual_disk, a
// volume on the Proxmox disk-owner VM), so replacing the VM preserves them - there is no backend on
// which a disk-bearing node has to be left drained-but-not-rebuilt, which is what the old refusal
// (and its drain-ordering hazard) existed to avoid.
func TestDiskBearingNodeIsRebuiltNotRefused(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &repairCfg{}
	r.Cfg = cfg
	r.RepairPolicy = reconcileRepairPolicy()

	c := readyClusterWithWorkers(t, r, st, "rp8", 2)
	worker := workerName(t, c)
	// Give the worker an extra disk - exactly the case the old code refused on vSphere/Proxmox.
	c.NodeDisks = []domain.NodeDisk{{
		VMName: worker, Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
		FSType: domain.FSExt4, Phase: domain.DiskPhaseAttached,
		WWN: domain.NewDiskWWN(c.ID, worker, "data"),
	}}
	if err := st.UpdateCluster(c); err != nil {
		t.Fatal(err)
	}

	since := time.Now().Add(-2 * time.Hour)
	acted := time.Now().Add(-90 * time.Minute)
	c.RepairState().Nodes = map[string]*domain.NodeRepairState{
		worker: {
			Fault: domain.FaultNotReady, UnhealthySince: &since, Attempts: 1,
			LastAction: domain.ActionRestartKubelet, LastActionAt: &acted,
		},
	}
	saveHealth(t, st, c, time.Now(), worker)

	if err := r.reconcileOne(context.Background(), c); err != nil { // decide: replace
		t.Fatal(err)
	}
	if c.RepairState().Action != domain.ActionReplace {
		t.Fatalf("action = %s, want replace", c.RepairState().Action)
	}
	if err := r.reconcileOne(context.Background(), c); err != nil { // execute: rebuild proceeds
		t.Fatal(err)
	}

	// The rebuild happened and was NOT refused: the worker is back and rejoined, its disk still desired.
	if got := c.RepairState().Nodes[worker]; got.Suspended {
		t.Fatal("a disk-bearing node was suspended - the refusal that should no longer exist")
	}
	if cfg.joinWorkers == 0 {
		t.Fatal("replaced worker was never rejoined - the rebuild did not proceed")
	}
	if len(c.NodeDisks) != 1 || c.NodeDisks[0].Name != "data" {
		t.Fatalf("NodeDisks = %+v, want the disk preserved across the rebuild", c.NodeDisks)
	}
	// The recorded operation is a normal repair, closed successfully - not a failure.
	ops := opsOfKind(t, st, c.ID, domain.OpRepair)
	if len(ops) != 1 || ops[0].Status != domain.OpCompleted {
		t.Fatalf("repair op = %+v, want one completed op", ops)
	}
	if strings.HasPrefix(ops[0].Detail, "failed:") {
		t.Fatalf("rebuild op recorded a failure: %q", ops[0].Detail)
	}
}
