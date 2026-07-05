package reconcile

import (
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// A cluster's VMs are named for the pool that owns them, sized by that pool, and stamped with it -
// which is what lets the rest of the loop stay pool-agnostic.
func TestReconcileProvisionsPoolShapedNodes(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{
		ID: "p1", Name: "demo", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
		NodePools: []domain.NodePool{
			{Name: "default", Size: "small", DesiredWorkers: 1},
			{Name: "gpu", Size: "large", DesiredWorkers: 1},
		},
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "p1")

	got, _ := st.GetCluster("p1")
	byName := map[string]domain.Node{}
	for _, n := range got.Nodes {
		byName[n.VMName] = n
	}
	for name, wantPool := range map[string]string{
		"demo-cp-0":      "",
		"demo-default-0": "default",
		"demo-gpu-0":     "gpu",
	} {
		n, ok := byName[name]
		if !ok {
			t.Fatalf("node %q missing; got %v", name, byName)
		}
		if n.Pool != wantPool {
			t.Errorf("node %s pool = %q, want %q", name, n.Pool, wantPool)
		}
	}
	if len(got.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(got.Nodes))
	}
}

// Removing a pool is not a special path: its nodes simply stop being desired, so the same
// drain-then-destroy the reconciler already does for a scale-down handles it.
func TestRemovingPoolDrainsItsNodes(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &recordingCfg{Fake: config.NewFake()}
	r.Cfg = cfg

	c := &domain.Cluster{
		ID: "p2", Name: "demo", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
		NodePools: []domain.NodePool{
			{Name: "default", Size: "small", DesiredWorkers: 1},
			{Name: "gpu", Size: "small", DesiredWorkers: 2},
		},
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "p2")
	if got, _ := st.GetCluster("p2"); len(got.Nodes) != 4 { // 1 cp + 1 default + 2 gpu
		t.Fatalf("after create: nodes = %d, want 4", len(got.Nodes))
	}

	// Drop the gpu pool entirely.
	got, _ := st.GetCluster("p2")
	got.NodePools = []domain.NodePool{{Name: "default", Size: "small", DesiredWorkers: 1}}
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "p2")

	after, _ := st.GetCluster("p2")
	if len(after.Nodes) != 2 { // 1 cp + 1 default
		t.Fatalf("after removing the gpu pool: nodes = %d, want 2: %+v", len(after.Nodes), after.Nodes)
	}
	for _, n := range after.Nodes {
		if n.Pool == "gpu" {
			t.Fatalf("node %s from the removed pool survived", n.VMName)
		}
	}
	// Every gpu node must have been drained before its VM was destroyed - otherwise the cluster
	// keeps a NotReady node object behind.
	want := map[string]bool{"demo-gpu-0": true, "demo-gpu-1": true}
	for _, name := range cfg.removed {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("these nodes were destroyed without being drained: %v (drained: %v)", want, cfg.removed)
	}
}

// Scaling one pool must leave the other's nodes exactly where they are - that's what "scaled
// independently" means, and the name-keyed convergence is what delivers it.
func TestScalingOnePoolLeavesOthersAlone(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{
		ID: "p3", Name: "demo", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
		NodePools: []domain.NodePool{
			{Name: "a", Size: "small", DesiredWorkers: 1},
			{Name: "b", Size: "small", DesiredWorkers: 1},
		},
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "p3")

	before, _ := st.GetCluster("p3")
	bIP := nodeByName(before.Nodes, "demo-b-0").IP
	if bIP == "" {
		t.Fatal("demo-b-0 has no IP after create")
	}

	got, _ := st.GetCluster("p3")
	got.NodePools = []domain.NodePool{
		{Name: "a", Size: "small", DesiredWorkers: 3},
		{Name: "b", Size: "small", DesiredWorkers: 1},
	}
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "p3")

	after, _ := st.GetCluster("p3")
	if got := nodeByName(after.Nodes, "demo-b-0").IP; got != bIP {
		t.Errorf("pool b's node moved from %s to %s when pool a was scaled", bIP, got)
	}
	if after.WorkerCount() != 4 {
		t.Fatalf("worker count = %d, want 4 (3 in a + 1 in b)", after.WorkerCount())
	}
}
