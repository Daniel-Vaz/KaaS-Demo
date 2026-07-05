package domain

import (
	"strings"
	"testing"
)

func TestDesiredNodesNamesAndSizes(t *testing.T) {
	c := &Cluster{
		Name:          "web",
		Size:          "small",
		ControlPlanes: 3,
		NodePools: []NodePool{
			{Name: "default", Size: "small", DesiredWorkers: 2},
			{Name: "gpu", Size: "large", DesiredWorkers: 1},
		},
	}
	got := DesiredNodes(c)
	want := []struct {
		name string
		role Role
		pool string
		cpus int
	}{
		{"web-cp-0", RoleControlPlane, "", 2},
		{"web-cp-1", RoleControlPlane, "", 2},
		{"web-cp-2", RoleControlPlane, "", 2},
		{"web-default-0", RoleWorker, "default", 2},
		{"web-default-1", RoleWorker, "default", 2},
		{"web-gpu-0", RoleWorker, "gpu", 8},
	}
	if len(got) != len(want) {
		t.Fatalf("DesiredNodes returned %d nodes, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.VMName != w.name || g.Role != w.role || g.Pool != w.pool || g.Spec.CPUs != w.cpus {
			t.Errorf("node %d = {%s %s pool=%q %dcpu}, want {%s %s pool=%q %dcpu}",
				i, g.VMName, g.Role, g.Pool, g.Spec.CPUs, w.name, w.role, w.pool, w.cpus)
		}
	}
}

// A worker's resources come from its OWN pool, not from the cluster's size (which is the control
// plane's). Getting this wrong would silently provision every worker at the wrong shape.
func TestDesiredNodesSizesWorkersFromTheirPool(t *testing.T) {
	c := &Cluster{
		Name: "x", Size: "small", ControlPlanes: 1,
		NodePools: []NodePool{{Name: "big", Size: "large", DesiredWorkers: 1}},
	}
	got := DesiredNodes(c)
	if got[0].Spec.MemMB != Sizes["small"].MemMB {
		t.Errorf("control plane sized %d MB, want the cluster's small (%d)", got[0].Spec.MemMB, Sizes["small"].MemMB)
	}
	if got[1].Spec.MemMB != Sizes["large"].MemMB {
		t.Errorf("worker sized %d MB, want its pool's large (%d)", got[1].Spec.MemMB, Sizes["large"].MemMB)
	}
}

func TestWorkerCountSumsPools(t *testing.T) {
	c := &Cluster{NodePools: []NodePool{
		{Name: "a", Size: "small", DesiredWorkers: 2},
		{Name: "b", Size: "small", DesiredWorkers: 3},
	}}
	if got := c.WorkerCount(); got != 5 {
		t.Fatalf("WorkerCount = %d, want 5", got)
	}
	if got := (&Cluster{}).WorkerCount(); got != 0 {
		t.Fatalf("WorkerCount (no pools) = %d, want 0", got)
	}
}

// A cluster with no pools is legal: pools are workers only, so this is a control-plane-only cluster.
func TestDesiredNodesNoPools(t *testing.T) {
	got := DesiredNodes(&Cluster{Name: "solo", Size: "small", ControlPlanes: 1})
	if len(got) != 1 || got[0].VMName != "solo-cp-0" {
		t.Fatalf("DesiredNodes(no pools) = %+v, want just the control plane", got)
	}
}

func TestNodeSize(t *testing.T) {
	c := &Cluster{
		Size:      "small",
		NodePools: []NodePool{{Name: "gpu", Size: "large", DesiredWorkers: 1}},
	}
	if got := c.NodeSize(Node{Role: RoleControlPlane}); got.CPUs != 2 {
		t.Errorf("control plane NodeSize = %d cpus, want the cluster's small (2)", got.CPUs)
	}
	if got := c.NodeSize(Node{Role: RoleWorker, Pool: "gpu"}); got.CPUs != 8 {
		t.Errorf("worker NodeSize = %d cpus, want its pool's large (8)", got.CPUs)
	}
	// A worker whose pool has been deleted is on its way out; fall back rather than report zero.
	if got := c.NodeSize(Node{Role: RoleWorker, Pool: "gone"}); got.CPUs != 2 {
		t.Errorf("orphaned worker NodeSize = %d cpus, want the cluster fallback (2)", got.CPUs)
	}
}

func TestValidatePoolName(t *testing.T) {
	cases := []struct {
		name    string
		pool    string
		wantErr string // substring; "" = must be accepted
	}{
		{"simple", "default", ""},
		{"dashes", "gpu-a100", ""},
		{"digits", "pool2", ""},
		{"empty", "", "required"},
		{"uppercase", "GPU", "lowercase"},
		{"underscore", "gpu_a", "lowercase"},
		{"leading dash", "-gpu", "lowercase"},
		{"trailing dash", "gpu-", "lowercase"},
		// "cp" would mint "<cluster>-cp-<i>", colliding with the control planes' own names.
		{"reserved cp", "cp", "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePoolName("web", tc.pool)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ValidatePoolName(%q) = %v, want accepted", tc.pool, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ValidatePoolName(%q) = nil, want an error about %q", tc.pool, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("ValidatePoolName(%q) = %v, want an error about %q", tc.pool, err, tc.wantErr)
			}
		})
	}
}

// A node's VM name is its Linux hostname and its Kubernetes node name, so cluster+pool together are
// what has to fit in 63 chars - neither is bounded on its own.
func TestValidatePoolNameLength(t *testing.T) {
	long := strings.Repeat("a", 40)
	if err := ValidatePoolName("short", long); err != nil {
		t.Fatalf("a 40-char pool on a short cluster name should fit: %v", err)
	}
	if err := ValidatePoolName(strings.Repeat("c", 40), long); err == nil {
		t.Fatal("a 40-char pool on a 40-char cluster name would blow the hostname limit, want a rejection")
	}
}

func TestValidateNodePools(t *testing.T) {
	ok := []NodePool{{Name: "default", Size: "small", DesiredWorkers: 0}, {Name: "gpu", Size: "large", DesiredWorkers: 2}}
	if err := ValidateNodePools("web", ok); err != nil {
		t.Fatalf("valid pools rejected: %v", err)
	}
	// An empty list is legal - a cluster may run no workers at all.
	if err := ValidateNodePools("web", nil); err != nil {
		t.Fatalf("an empty pool list should be valid: %v", err)
	}
	dup := []NodePool{{Name: "a", Size: "small"}, {Name: "a", Size: "small"}}
	if err := ValidateNodePools("web", dup); err == nil {
		t.Fatal("duplicate pool names should be rejected - they'd mint colliding VM names")
	}
	badSize := []NodePool{{Name: "a", Size: "enormous"}}
	if err := ValidateNodePools("web", badSize); err == nil {
		t.Fatal("an unknown pool size should be rejected")
	}
	negative := []NodePool{{Name: "a", Size: "small", DesiredWorkers: -1}}
	if err := ValidateNodePools("web", negative); err == nil {
		t.Fatal("a negative worker count should be rejected")
	}
}
