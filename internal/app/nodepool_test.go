package app

import (
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// newPoolApp is a tenancy app plus a registered user granted 8 vCPU on kvm - enough for a small
// control plane (2) plus 6 vCPU of workers, which is what the quota assertions below lean on.
func newPoolApp(t *testing.T) (*App, *domain.User) {
	t.Helper()
	a := newTenancyApp(t)
	alice, err := a.Register("alice", "password")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	updated, err := a.UpdateUser(admin(t, a), alice.ID, UpdateUserRequest{
		Quotas: &map[string]domain.ResourceQuota{domain.ProviderKVM: {VCPU: 8, MemMB: 49152, DiskGB: 4096}},
	})
	if err != nil {
		t.Fatalf("grant quota: %v", err)
	}
	return a, updated
}

// Every cluster is born with a "default" pool, so a caller that never mentions pools still gets the
// one-worker-group cluster they expect.
func TestCreateEnsuresDefaultPool(t *testing.T) {
	a, alice := newPoolApp(t)

	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.NodePools) != 1 || c.NodePools[0].Name != domain.DefaultPoolName {
		t.Fatalf("node pools = %+v, want one named %q", c.NodePools, domain.DefaultPoolName)
	}
	if c.NodePools[0].Size != "small" {
		t.Errorf("default pool size = %q, want the cluster's own size", c.NodePools[0].Size)
	}
}

// A caller who names extra pools gets the default prepended, not replaced...
func TestCreatePrependsDefaultAlongsideNamedPools(t *testing.T) {
	a, alice := newPoolApp(t)

	c, err := a.CreateCluster(alice, CreateRequest{
		Name: "dev", Size: "small",
		NodePools: []domain.NodePool{{Name: "gpu", Size: "small", DesiredWorkers: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.NodePools) != 2 || c.NodePools[0].Name != "default" || c.NodePools[1].Name != "gpu" {
		t.Fatalf("node pools = %+v, want [default gpu]", c.NodePools)
	}
}

// ...but a caller who names "default" themselves keeps theirs verbatim, size and all.
func TestCreateKeepsCallerSuppliedDefaultPool(t *testing.T) {
	a, alice := newPoolApp(t)

	c, err := a.CreateCluster(alice, CreateRequest{
		Name: "dev", Size: "small",
		NodePools: []domain.NodePool{{Name: "default", Size: "medium", DesiredWorkers: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.NodePools) != 1 || c.NodePools[0].Size != "medium" {
		t.Fatalf("node pools = %+v, want the caller's own default at medium", c.NodePools)
	}
}

func TestCreateRejectsInvalidPool(t *testing.T) {
	a, alice := newPoolApp(t)

	_, err := a.CreateCluster(alice, CreateRequest{
		Name: "dev", Size: "small",
		NodePools: []domain.NodePool{{Name: "cp", Size: "small", DesiredWorkers: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("a pool named \"cp\" collides with control-plane node names; err = %v", err)
	}
}

// Scaling, adding and removing are all the same declarative edit: send the pool list you want.
func TestUpdateNodePoolsLifecycle(t *testing.T) {
	a, alice := newPoolApp(t)
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}

	// Add a pool alongside the default, and scale the default up.
	up, err := a.UpdateCluster(alice, c.ID, UpdateRequest{NodePools: ptr([]domain.NodePool{
		{Name: "default", Size: "small", DesiredWorkers: 2},
		{Name: "gpu", Size: "small", DesiredWorkers: 1},
	})})
	if err != nil {
		t.Fatal(err)
	}
	if up.WorkerCount() != 3 {
		t.Fatalf("worker count = %d, want 3 across both pools", up.WorkerCount())
	}
	if up.Generation <= c.Generation {
		t.Fatal("a pool edit must bump the generation - it's the level-triggered signal")
	}

	// Remove the default outright, keeping only gpu. Nothing protects the default pool once created.
	down, err := a.UpdateCluster(alice, up.ID, UpdateRequest{NodePools: ptr([]domain.NodePool{
		{Name: "gpu", Size: "small", DesiredWorkers: 1},
	})})
	if err != nil {
		t.Fatal(err)
	}
	if len(down.NodePools) != 1 || down.NodePools[0].Name != "gpu" {
		t.Fatalf("node pools = %+v, want just gpu - the default is deletable like any other", down.NodePools)
	}

	// And a cluster may end up with no pools at all (control plane only).
	none, err := a.UpdateCluster(alice, down.ID, UpdateRequest{NodePools: ptr([]domain.NodePool{})})
	if err != nil {
		t.Fatal(err)
	}
	if none.WorkerCount() != 0 {
		t.Fatalf("worker count = %d, want 0", none.WorkerCount())
	}
}

// A no-op pool edit must not bump the generation, or every save would churn the reconciler.
func TestUpdateNodePoolsNoOp(t *testing.T) {
	a, alice := newPoolApp(t)
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small", NodePools: pools(1)})
	if err != nil {
		t.Fatal(err)
	}
	same, err := a.UpdateCluster(alice, c.ID, UpdateRequest{NodePools: ptr(pools(1))})
	if err != nil {
		t.Fatal(err)
	}
	if same.Generation != c.Generation {
		t.Fatalf("generation moved %d → %d on a no-op pool edit", c.Generation, same.Generation)
	}
}

// A pool's size is fixed for its lifetime: changing it would mean rolling every node in it, so the
// edit is refused rather than silently acted on.
func TestUpdateRejectsPoolSizeChange(t *testing.T) {
	a, alice := newPoolApp(t)
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small", NodePools: pools(1)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.UpdateCluster(alice, c.ID, UpdateRequest{NodePools: ptr([]domain.NodePool{
		{Name: "default", Size: "large", DesiredWorkers: 1},
	})})
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("changing an existing pool's size should be rejected; err = %v", err)
	}
}

// Quota is charged on the candidate's whole shape, so a pool at a bigger size is priced at THAT
// size - not at the cluster's.
func TestUpdateNodePoolsChargesPoolSize(t *testing.T) {
	a, alice := newPoolApp(t)
	// alice holds 8 vCPU on kvm (see newPoolApp): a small control plane (2) leaves 6.
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	// One LARGE worker is 8 vCPU - past the remaining 6, even though it is a single node.
	_, err = a.UpdateCluster(alice, c.ID, UpdateRequest{NodePools: ptr([]domain.NodePool{
		{Name: "default", Size: "small", DesiredWorkers: 0},
		{Name: "big", Size: "large", DesiredWorkers: 1},
	})})
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("a large pool must be priced at large; err = %v", err)
	}
	// Two small workers (4 vCPU) fit in the same 6.
	if _, err := a.UpdateCluster(alice, c.ID, UpdateRequest{NodePools: ptr(pools(2))}); err != nil {
		t.Fatalf("2 small workers should fit the remaining quota: %v", err)
	}
}
