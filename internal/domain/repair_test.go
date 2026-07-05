package domain

import (
	"testing"
	"time"
)

// repairTestPolicy is a fully-enabled policy with round thresholds, so a test reads as a statement about
// the guard it is exercising rather than about arithmetic.
func repairTestPolicy() RepairPolicy {
	return RepairPolicy{
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

// repairTestCluster builds a Ready, converged cluster with one control plane and the given worker names.
func repairTestCluster(workers ...string) *Cluster {
	c := &Cluster{
		ID: "c1", Name: "test", Phase: PhaseReady, Generation: 1, ObservedGeneration: 1,
		ControlPlanes: 1,
		Nodes:         []Node{{Role: RoleControlPlane, VMName: "test-cp-0", IP: "10.0.0.10"}},
	}
	for i, w := range workers {
		c.Nodes = append(c.Nodes, Node{Role: RoleWorker, VMName: w, IP: "10.0.0.2" + string(rune('0'+i))})
	}
	return c
}

// healthy builds a health snapshot reporting every named node Ready and the API server up.
func healthy(at time.Time, nodes ...string) *HealthSnapshot {
	s := &HealthSnapshot{CollectedAt: at, Checks: []HealthCheck{{ID: "api-server", Status: HealthHealthy}}}
	for _, n := range nodes {
		s.Nodes = append(s.Nodes, NodeHealth{NodeName: n, Ready: true})
	}
	return s
}

// notReady marks one of the snapshot's nodes NotReady.
func notReady(s *HealthSnapshot, name string) *HealthSnapshot {
	for i := range s.Nodes {
		if s.Nodes[i].NodeName == name {
			s.Nodes[i].Ready = false
		}
	}
	return s
}

// TestUnobservableClusterIsNotBroken is the single most important guard here: when the API server is
// unreachable every node reads NotReady, and a repairer that believes that would rebuild a healthy
// cluster during a network partition.
func TestUnobservableClusterIsNotBroken(t *testing.T) {
	now := time.Now()
	c := repairTestCluster("test-wk-0", "test-wk-1")
	snap := healthy(now, "test-cp-0", "test-wk-0", "test-wk-1")
	// The API server is down, so every per-node reading in this snapshot is worthless.
	snap.Checks = []HealthCheck{{ID: "api-server", Status: HealthUnhealthy}}
	for i := range snap.Nodes {
		snap.Nodes[i].Ready = false
	}
	obs := RepairObservation{Health: snap, Now: now}

	faults, observable := ObserveFaults(c, obs, repairTestPolicy())
	if observable {
		t.Fatal("cluster with an unreachable API server reported observable")
	}
	if len(faults) != 0 {
		t.Fatalf("got %d faults from an unobservable cluster, want 0: %v", len(faults), faults)
	}
}

// TestStaleHealthIsNotObservable: a snapshot nobody has refreshed is "nobody looked recently", not
// "nothing is wrong" - and equally not "everything is broken".
func TestStaleHealthIsNotObservable(t *testing.T) {
	now := time.Now()
	c := repairTestCluster("test-wk-0")
	snap := notReady(healthy(now.Add(-time.Hour), "test-cp-0", "test-wk-0"), "test-wk-0")

	_, observable := ObserveFaults(c, RepairObservation{Health: snap, Now: now}, repairTestPolicy())
	if observable {
		t.Fatal("hour-old health snapshot reported observable under a 1m HealthMaxAge")
	}
}

// TestPowerStateSurvivesBlindness is the corroboration rule: a VM the hypervisor says is off is a
// fault observed BELOW the layer that has gone quiet, so it stands even when the cluster does not
// answer. Without this, a sole-control-plane cluster could never be repaired - its API server going
// away is precisely the symptom.
func TestPowerStateSurvivesBlindness(t *testing.T) {
	now := time.Now()
	c := repairTestCluster("test-wk-0")
	obs := RepairObservation{
		Health:  &HealthSnapshot{CollectedAt: now, Checks: []HealthCheck{{ID: "api-server", Status: HealthUnhealthy}}},
		Powered: map[string]bool{"test-cp-0": false, "test-wk-0": true},
		Now:     now,
	}

	faults, observable := ObserveFaults(c, obs, repairTestPolicy())
	if observable {
		t.Fatal("expected the cluster to be unobservable")
	}
	if faults["test-cp-0"] != FaultVMDown {
		t.Fatalf("control plane fault = %q, want %q", faults["test-cp-0"], FaultVMDown)
	}
	if _, ok := faults["test-wk-0"]; ok {
		t.Fatal("a powered-on worker should carry no fault when the cluster is unobservable")
	}
}

// TestNoHealthSnapshotIsNotObservable covers a cluster whose health has never been evaluated.
func TestNoHealthSnapshotIsNotObservable(t *testing.T) {
	c := repairTestCluster("test-wk-0")
	if _, observable := ObserveFaults(c, RepairObservation{Now: time.Now()}, repairTestPolicy()); observable {
		t.Fatal("cluster with no health snapshot reported observable")
	}
}

// TestBlastRadiusStandsDown: past the threshold this is one cluster-wide fault, not N node faults,
// and rebuilding nodes makes it worse.
func TestBlastRadiusStandsDown(t *testing.T) {
	now := time.Now()
	p := repairTestPolicy()
	c := repairTestCluster("test-wk-0", "test-wk-1")
	snap := healthy(now, "test-cp-0", "test-wk-0", "test-wk-1")
	notReady(snap, "test-wk-0")
	notReady(snap, "test-wk-1")
	obs := RepairObservation{Health: snap, Now: now}

	faults, observable := ObserveFaults(c, obs, p)
	if len(faults) != 2 {
		t.Fatalf("got %d faults, want 2", len(faults))
	}
	// Two of three nodes is 0.66, over the 0.5 limit.
	plan, reason := p.Plan(c, obs, faults, observable)
	if plan.Act() {
		t.Fatalf("planned %s on %s despite 2/3 nodes faulty", plan.Action, plan.Target)
	}
	if reason == "" {
		t.Fatal("expected a reason for standing down")
	}
}

// TestFleetBreakerStandsDown is the guard no per-cluster check can replace: when the worker loses
// the hypervisor, every cluster goes unhealthy at once and each looks locally repairable.
func TestFleetBreakerStandsDown(t *testing.T) {
	now := time.Now()
	p := repairTestPolicy()
	c := repairTestCluster("test-wk-0")
	snap := notReady(healthy(now, "test-cp-0", "test-wk-0"), "test-wk-0")
	obs := RepairObservation{Health: snap, Now: now, FleetUnhealthy: 0.9, FleetUnhealthyN: 5}

	faults, observable := ObserveFaults(c, obs, p)
	c.RepairState()
	MergeFaults(c.Repair, c, faults, p, now.Add(-time.Hour)) // long past the grace
	if plan, _ := p.Plan(c, obs, faults, observable); plan.Act() {
		t.Fatalf("planned %s with 90%% of the fleet unhealthy", plan.Action)
	}
}

// TestFleetBreakerNeedsTwoUnhealthyClusters: a single unhealthy cluster is 100% of a one-cluster
// fleet, which is a cluster fault the per-cluster guards handle - not a fleet fault. Inferring a
// platform-wide outage from a sample of one would make repair a no-op on every small deployment.
func TestFleetBreakerNeedsTwoUnhealthyClusters(t *testing.T) {
	now := time.Now()
	p := repairTestPolicy()
	c := repairTestCluster("test-wk-0")
	// Fault already past the grace, so a repair would be planned if the breaker allows it.
	start := now.Add(-15 * time.Minute)
	snap := notReady(healthy(now, "test-cp-0", "test-wk-0"), "test-wk-0")
	// The whole fleet is "100% unhealthy" - but that whole fleet is this one cluster.
	obs := RepairObservation{Health: snap, Now: now, FleetUnhealthy: 1.0, FleetUnhealthyN: 1}
	faults, observable := ObserveFaults(c, obs, p)
	MergeFaults(c.RepairState(), c, faults, p, start)

	plan, reason := p.Plan(c, obs, faults, observable)
	if plan.Action != ActionRestartKubelet {
		t.Fatalf("plan = %s (%s), want restart-kubelet - a one-cluster fleet must not trip the breaker", plan.Action, reason)
	}
}

// TestNotReadyGraceThenRestartKubelet walks the cheap rung: nothing happens inside the grace, and a
// kubelet restart is chosen once the fault has outlived it.
func TestNotReadyGraceThenRestartKubelet(t *testing.T) {
	p := repairTestPolicy()
	start := time.Now()
	c := repairTestCluster("test-wk-0")
	snap := notReady(healthy(start, "test-cp-0", "test-wk-0"), "test-wk-0")
	obs := RepairObservation{Health: snap, Now: start}

	faults, observable := ObserveFaults(c, obs, p)
	MergeFaults(c.RepairState(), c, faults, p, start)

	if plan, _ := p.Plan(c, obs, faults, observable); plan.Act() {
		t.Fatalf("planned %s immediately; the NotReady grace should hold", plan.Action)
	}

	// Same fault, eleven minutes later.
	later := start.Add(11 * time.Minute)
	snap.CollectedAt = later
	obs = RepairObservation{Health: snap, Now: later}
	faults, observable = ObserveFaults(c, obs, p)
	MergeFaults(c.Repair, c, faults, p, later)
	if since := c.Repair.Nodes["test-wk-0"].UnhealthySince; since == nil || !since.Equal(start) {
		t.Fatalf("UnhealthySince = %v, want it pinned to the first observation %v", since, start)
	}
	plan, reason := p.Plan(c, obs, faults, observable)
	if plan.Action != ActionRestartKubelet || plan.Target != "test-wk-0" {
		t.Fatalf("plan = %s on %s (%s), want restart-kubelet on test-wk-0", plan.Action, plan.Target, reason)
	}
}

// TestEscalatesToReplace: the cheap rung was tried, the fault outlived ReplaceAfter, so the node is
// rebuilt.
func TestEscalatesToReplace(t *testing.T) {
	p := repairTestPolicy()
	start := time.Now()
	c := repairTestCluster("test-wk-0")
	since := start
	acted := start.Add(time.Minute)
	c.Repair = &ClusterRepair{Nodes: map[string]*NodeRepairState{
		"test-wk-0": {Fault: FaultNotReady, UnhealthySince: &since, Attempts: 1,
			LastAction: ActionRestartKubelet, LastActionAt: &acted},
	}}
	now := start.Add(45 * time.Minute) // past ReplaceAfter and past the first backoff
	snap := notReady(healthy(now, "test-cp-0", "test-wk-0"), "test-wk-0")
	obs := RepairObservation{Health: snap, Now: now}
	faults, observable := ObserveFaults(c, obs, p)

	plan, reason := p.Plan(c, obs, faults, observable)
	if plan.Action != ActionReplace {
		t.Fatalf("plan = %s (%s), want replace", plan.Action, reason)
	}
}

// TestBackoffHoldsBetweenAttempts: the ladder must not fire again the moment a rung returns.
func TestBackoffHoldsBetweenAttempts(t *testing.T) {
	p := repairTestPolicy()
	start := time.Now()
	c := repairTestCluster("test-wk-0")
	since := start
	acted := start.Add(40 * time.Minute)
	c.Repair = &ClusterRepair{Nodes: map[string]*NodeRepairState{
		"test-wk-0": {Fault: FaultNotReady, UnhealthySince: &since, Attempts: 1,
			LastAction: ActionRestartKubelet, LastActionAt: &acted},
	}}
	now := start.Add(50 * time.Minute) // past ReplaceAfter, but only 10m since the last attempt
	snap := notReady(healthy(now, "test-cp-0", "test-wk-0"), "test-wk-0")
	obs := RepairObservation{Health: snap, Now: now}
	faults, observable := ObserveFaults(c, obs, p)

	if plan, reason := p.Plan(c, obs, faults, observable); plan.Act() {
		t.Fatalf("planned %s inside the 30m backoff (%s)", plan.Action, reason)
	}
}

// TestSuspendsAfterMaxAttempts: a repair loop is worse than the fault, so the platform gives up
// loudly rather than rebuilding a doomed node forever.
func TestSuspendsAfterMaxAttempts(t *testing.T) {
	p := repairTestPolicy()
	now := time.Now()
	r := &ClusterRepair{}
	for i := 1; i <= p.MaxAttempts; i++ {
		r.RecordAttempt(RepairPlan{Target: "test-wk-0", Action: ActionReplace}, p, now)
		r.CompleteAttempt()
	}
	st := r.Nodes["test-wk-0"]
	if !st.Suspended {
		t.Fatalf("node not suspended after %d attempts", st.Attempts)
	}

	// And a suspended node is never planned for again.
	c := repairTestCluster("test-wk-0")
	c.Repair = r
	since := now.Add(-2 * time.Hour)
	st.Fault, st.UnhealthySince, st.LastActionAt = FaultNotReady, &since, nil
	snap := notReady(healthy(now, "test-cp-0", "test-wk-0"), "test-wk-0")
	obs := RepairObservation{Health: snap, Now: now}
	faults, observable := ObserveFaults(c, obs, p)
	if plan, _ := p.Plan(c, obs, faults, observable); plan.Act() {
		t.Fatalf("planned %s on a suspended node", plan.Action)
	}
}

// TestRecordAttemptPrecedesWork pins the ordering that keeps the give-up counter honest: the attempt
// is stamped before the repair runs, so a crash mid-flight still counts.
func TestRecordAttemptPrecedesWork(t *testing.T) {
	p := repairTestPolicy()
	now := time.Now()
	r := &ClusterRepair{}
	r.RecordAttempt(RepairPlan{Target: "n", Action: ActionReplace}, p, now)
	st := r.Nodes["n"]
	if st.Attempts != 1 || st.LastAction != ActionReplace || st.LastActionAt == nil {
		t.Fatalf("attempt not stamped: %+v", st)
	}
	if !r.InFlight() || r.Target != "n" {
		t.Fatal("plan not marked in flight")
	}
	r.CompleteAttempt()
	if r.InFlight() {
		t.Fatal("plan still in flight after completion")
	}
	// Crucially, completing does NOT clear the fault: whether the repair worked is decided by the
	// next observation, not by the action returning.
	if r.Nodes["n"].Attempts != 1 {
		t.Fatal("completing an attempt reset the counter")
	}
}

// TestSoleControlPlaneRestoresRatherThanReplaces: there is nothing to drain it from and no quorum
// member to copy state off, so the only rebuild available is the lossy one.
func TestSoleControlPlaneRestoresRatherThanReplaces(t *testing.T) {
	p := repairTestPolicy()
	start := time.Now()
	c := &Cluster{
		ID: "c1", Phase: PhaseReady, Generation: 1, ObservedGeneration: 1, ControlPlanes: 1,
		Nodes: []Node{{Role: RoleControlPlane, VMName: "test-cp-0", IP: "10.0.0.10"}},
	}
	since := start
	acted := start.Add(time.Minute)
	c.Repair = &ClusterRepair{Nodes: map[string]*NodeRepairState{
		"test-cp-0": {Fault: FaultVMDown, UnhealthySince: &since, Attempts: 1,
			LastAction: ActionPowerOn, LastActionAt: &acted},
	}}
	now := start.Add(45 * time.Minute)
	obs := RepairObservation{
		Health:  &HealthSnapshot{CollectedAt: now, Checks: []HealthCheck{{ID: "api-server", Status: HealthUnhealthy}}},
		Powered: map[string]bool{"test-cp-0": false},
		Now:     now,
	}
	faults, observable := ObserveFaults(c, obs, p)

	plan, reason := p.Plan(c, obs, faults, observable)
	if plan.Action != ActionRestore {
		t.Fatalf("plan = %s (%s), want restore for a dead sole control plane", plan.Action, reason)
	}

	// With restores disabled it refuses rather than silently falling back to a plain replacement,
	// which would rebuild an empty control plane over the cluster's state.
	p.Restore = false
	if plan, _ := p.Plan(c, obs, faults, observable); plan.Act() {
		t.Fatalf("planned %s with restore disabled", plan.Action)
	}
}

// TestQuorumGuardRefusesControlPlaneReplacement: defragmentation refuses to run while a member is
// unreachable, and replacing a control plane is strictly more disruptive.
func TestQuorumGuardRefusesControlPlaneReplacement(t *testing.T) {
	p := repairTestPolicy()
	start := time.Now()
	c := &Cluster{
		ID: "c1", Phase: PhaseReady, Generation: 1, ObservedGeneration: 1, ControlPlanes: 3,
		Nodes: []Node{
			{Role: RoleControlPlane, VMName: "test-cp-0", IP: "10.0.0.10"},
			{Role: RoleControlPlane, VMName: "test-cp-1", IP: "10.0.0.11"},
			{Role: RoleControlPlane, VMName: "test-cp-2", IP: "10.0.0.12"},
		},
		// Only two of three members answered the last status read: one is already gone.
		Etcd: &EtcdStatus{Members: 2, ObservedAt: start},
	}
	since := start
	acted := start.Add(time.Minute)
	c.Repair = &ClusterRepair{Nodes: map[string]*NodeRepairState{
		"test-cp-0": {Fault: FaultNotReady, UnhealthySince: &since, Attempts: 1,
			LastAction: ActionRestartKubelet, LastActionAt: &acted},
	}}
	now := start.Add(45 * time.Minute)
	snap := healthy(now, "test-cp-0", "test-cp-1", "test-cp-2")
	notReady(snap, "test-cp-0")
	obs := RepairObservation{Health: snap, Now: now}
	faults, observable := ObserveFaults(c, obs, p)

	plan, reason := p.Plan(c, obs, faults, observable)
	if plan.Act() {
		t.Fatalf("planned %s on a control plane while only 2/3 etcd members are reachable", plan.Action)
	}
	if reason == "" {
		t.Fatal("expected the quorum refusal to explain itself")
	}
}

// TestWorkersBeforeControlPlanes: the same ordering rolling replacement uses, and for the same
// reason - control planes are the riskiest thing to touch.
func TestWorkersBeforeControlPlanes(t *testing.T) {
	p := repairTestPolicy()
	start := time.Now()
	c := &Cluster{
		ID: "c1", Phase: PhaseReady, Generation: 1, ObservedGeneration: 1, ControlPlanes: 3,
		Nodes: []Node{
			{Role: RoleControlPlane, VMName: "test-cp-0", IP: "10.0.0.10"},
			{Role: RoleControlPlane, VMName: "test-cp-1", IP: "10.0.0.11"},
			{Role: RoleControlPlane, VMName: "test-cp-2", IP: "10.0.0.12"},
			{Role: RoleWorker, VMName: "test-wk-0", IP: "10.0.0.20"},
		},
	}
	now := start.Add(11 * time.Minute)
	snap := healthy(now, "test-cp-0", "test-cp-1", "test-cp-2", "test-wk-0")
	notReady(snap, "test-cp-0")
	notReady(snap, "test-wk-0")
	obs := RepairObservation{Health: snap, Now: now}
	faults, observable := ObserveFaults(c, obs, p)
	MergeFaults(c.RepairState(), c, faults, p, start)

	plan, _ := p.Plan(c, obs, faults, observable)
	if plan.Target != "test-wk-0" {
		t.Fatalf("plan targets %s, want the worker first", plan.Target)
	}
}

// TestMissingNodeGetsStartupGraceThenRejoin: a booting node is not a broken one, and the fix for a
// node that never joined is to run the join again, not to rebuild it.
func TestMissingNodeGetsStartupGraceThenRejoin(t *testing.T) {
	p := repairTestPolicy()
	start := time.Now()
	c := repairTestCluster("test-wk-0")
	// The cluster only knows about the control plane; the worker's VM exists but never registered.
	snap := healthy(start, "test-cp-0")
	obs := RepairObservation{Health: snap, Now: start}
	faults, observable := ObserveFaults(c, obs, p)
	if faults["test-wk-0"] != FaultMissing {
		t.Fatalf("fault = %q, want %q", faults["test-wk-0"], FaultMissing)
	}
	MergeFaults(c.RepairState(), c, faults, p, start)
	if plan, _ := p.Plan(c, obs, faults, observable); plan.Act() {
		t.Fatalf("planned %s inside the startup grace", plan.Action)
	}

	now := start.Add(21 * time.Minute)
	snap.CollectedAt = now
	obs = RepairObservation{Health: snap, Now: now}
	faults, observable = ObserveFaults(c, obs, p)
	MergeFaults(c.Repair, c, faults, p, now)
	plan, reason := p.Plan(c, obs, faults, observable)
	if plan.Action != ActionRejoin {
		t.Fatalf("plan = %s (%s), want rejoin", plan.Action, reason)
	}
}

// TestNodeWithoutIPIsNotMissing: a node still being provisioned has no address yet and must not be
// mistaken for one that failed to join.
func TestNodeWithoutIPIsNotMissing(t *testing.T) {
	now := time.Now()
	c := repairTestCluster()
	c.Nodes = append(c.Nodes, Node{Role: RoleWorker, VMName: "test-wk-0"}) // no IP
	faults, _ := ObserveFaults(c, RepairObservation{Health: healthy(now, "test-cp-0"), Now: now}, repairTestPolicy())
	if _, ok := faults["test-wk-0"]; ok {
		t.Fatal("a node with no IP yet was reported as missing")
	}
}

// TestRecoveryClearsStateAndFlappingCarriesAttempts: recovery must not be an unlimited supply of
// fresh attempts, or "give up loudly" has a door left open in it.
func TestRecoveryClearsStateAndFlappingCarriesAttempts(t *testing.T) {
	p := repairTestPolicy()
	start := time.Now()
	c := repairTestCluster("test-wk-0")
	since := start
	c.Repair = &ClusterRepair{Nodes: map[string]*NodeRepairState{
		"test-wk-0": {Fault: FaultNotReady, UnhealthySince: &since, Attempts: 2},
	}}

	// Recovered.
	recovered := start.Add(20 * time.Minute)
	MergeFaults(c.Repair, c, map[string]NodeFault{}, p, recovered)
	st := c.Repair.Nodes["test-wk-0"]
	if st.Fault != "" || st.UnhealthySince != nil {
		t.Fatalf("recovery did not clear the fault: %+v", st)
	}
	if st.RepairedAt == nil || st.Attempts != 2 {
		t.Fatalf("recovery dropped the flap breadcrumb: %+v", st)
	}

	// Fails again inside the flap window: attempts carry over rather than resetting.
	again := recovered.Add(10 * time.Minute)
	MergeFaults(c.Repair, c, map[string]NodeFault{"test-wk-0": FaultNotReady}, p, again)
	if got := c.Repair.Nodes["test-wk-0"].Attempts; got != 2 {
		t.Fatalf("attempts after a flap = %d, want 2 carried forward", got)
	}

	// A node that stays healthy well past the window is forgotten entirely.
	MergeFaults(c.Repair, c, map[string]NodeFault{}, p, again)
	MergeFaults(c.Repair, c, map[string]NodeFault{}, p, again.Add(10*p.flapWindow()))
	if _, ok := c.Repair.Nodes["test-wk-0"]; ok {
		t.Fatal("a long-healthy node kept its repair state forever")
	}
}

// TestMergeFaultsDropsDepartedNodes: state for a node nothing converges is state that wedges later
// edits, the same reasoning that prunes node disks off departing nodes.
func TestMergeFaultsDropsDepartedNodes(t *testing.T) {
	p := repairTestPolicy()
	now := time.Now()
	c := repairTestCluster("test-wk-0")
	since := now
	c.Repair = &ClusterRepair{Nodes: map[string]*NodeRepairState{
		"test-wk-9": {Fault: FaultNotReady, UnhealthySince: &since},
	}}
	MergeFaults(c.Repair, c, map[string]NodeFault{}, p, now)
	if _, ok := c.Repair.Nodes["test-wk-9"]; ok {
		t.Fatal("kept repair state for a node that is no longer desired")
	}
}

// TestRepairDueOnlyWhenReadyAndConverged: during an update or an upgrade, nodes are drained, removed
// and rebuilt ON PURPOSE, and every one of them looks exactly like the faults this repairs.
func TestRepairDueOnlyWhenReadyAndConverged(t *testing.T) {
	p := repairTestPolicy()
	now := time.Now()
	c := repairTestCluster("test-wk-0")
	if !c.RepairDue(p, now) {
		t.Fatal("a Ready, converged, never-observed cluster should be due")
	}
	c.Phase = PhaseUpgrading
	if c.RepairDue(p, now) {
		t.Fatal("repair was due on an Upgrading cluster")
	}
	c.Phase = PhaseReady
	c.Generation = 2
	if c.RepairDue(p, now) {
		t.Fatal("repair was due on a cluster with pending desired-state changes")
	}
	c.Generation = 1
	c.Repair = &ClusterRepair{ObservedAt: now}
	if c.RepairDue(p, now) {
		t.Fatal("repair was due immediately after an observation")
	}
	if !c.RepairDue(p, now.Add(3*time.Minute)) {
		t.Fatal("repair was not due after the observe interval elapsed")
	}
}

// TestDisabledPolicyDoesNothing.
func TestDisabledPolicyDoesNothing(t *testing.T) {
	var p RepairPolicy // zero value: disabled
	now := time.Now()
	c := repairTestCluster("test-wk-0")
	if c.RepairDue(p, now) {
		t.Fatal("a disabled policy reported work due")
	}
	if plan, _ := p.Plan(c, RepairObservation{Now: now}, map[string]NodeFault{"test-wk-0": FaultNotReady}, true); plan.Act() {
		t.Fatal("a disabled policy planned a repair")
	}
}

// TestReplaceGateBlocksOnlyTheDestructiveRung.
func TestReplaceGateBlocksOnlyTheDestructiveRung(t *testing.T) {
	p := repairTestPolicy()
	p.Replace = false
	start := time.Now()
	c := repairTestCluster("test-wk-0")
	since := start
	acted := start.Add(time.Minute)
	c.Repair = &ClusterRepair{Nodes: map[string]*NodeRepairState{
		"test-wk-0": {Fault: FaultNotReady, UnhealthySince: &since, Attempts: 1,
			LastAction: ActionRestartKubelet, LastActionAt: &acted},
	}}
	now := start.Add(45 * time.Minute)
	snap := notReady(healthy(now, "test-cp-0", "test-wk-0"), "test-wk-0")
	obs := RepairObservation{Health: snap, Now: now}
	faults, observable := ObserveFaults(c, obs, p)
	if plan, _ := p.Plan(c, obs, faults, observable); plan.Act() {
		t.Fatalf("planned %s with replacement disabled", plan.Action)
	}

	// The cheap rung is unaffected: a fresh fault still gets a kubelet restart.
	c.Repair = &ClusterRepair{}
	MergeFaults(c.Repair, c, faults, p, start)
	faults, observable = ObserveFaults(c, obs, p)
	MergeFaults(c.Repair, c, faults, p, now)
	c.Repair.Nodes["test-wk-0"].UnhealthySince = &since
	if plan, _ := p.Plan(c, obs, faults, observable); plan.Action != ActionRestartKubelet {
		t.Fatalf("plan = %s, want restart-kubelet to remain available", plan.Action)
	}
}

// TestBackoffDoublesAndCaps.
func TestBackoffDoublesAndCaps(t *testing.T) {
	p := RepairPolicy{Backoff: 10 * time.Minute}
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{0, 0},
		{1, 10 * time.Minute},
		{2, 20 * time.Minute},
		{3, 40 * time.Minute},
		{99, 24 * time.Hour},
	} {
		if got := p.backoffFor(tc.attempts); got != tc.want {
			t.Errorf("backoffFor(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

// TestSingleNodeClusterIsStillRepairable pins the clause that makes the blast-radius guard require at
// least TWO faults. Its premise is that many simultaneous faults share one cause - and one fault has
// no "many" to infer that from, however large a share of the cluster it happens to be. Without the
// clause a sole-control-plane cluster is 100% faulty the instant its only node breaks, and could
// therefore never be repaired: precisely the case the snapshot-and-restore path exists for.
func TestSingleNodeClusterIsStillRepairable(t *testing.T) {
	p := repairTestPolicy()
	start := time.Now()
	c := &Cluster{
		ID: "c1", Phase: PhaseReady, Generation: 1, ObservedGeneration: 1, ControlPlanes: 1,
		Nodes: []Node{{Role: RoleControlPlane, VMName: "test-cp-0", IP: "10.0.0.10"}},
	}
	now := start.Add(time.Minute)
	obs := RepairObservation{
		Health:  &HealthSnapshot{CollectedAt: now, Checks: []HealthCheck{{ID: "api-server", Status: HealthUnhealthy}}},
		Powered: map[string]bool{"test-cp-0": false},
		Now:     now,
	}
	faults, observable := ObserveFaults(c, obs, p)
	MergeFaults(c.RepairState(), c, faults, p, now)

	plan, reason := p.Plan(c, obs, faults, observable)
	if plan.Action != ActionPowerOn {
		t.Fatalf("plan = %s (%s), want power-on - a 1/1 fault must not trip the blast-radius guard", plan.Action, reason)
	}
}
