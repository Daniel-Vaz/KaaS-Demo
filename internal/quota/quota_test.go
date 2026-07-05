package quota

import (
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// grants is a shorthand for a user's per-infrastructure quota map, as
// (provider, vcpu, memMB, diskGB) tuples.
func grants(pairs ...any) map[string]domain.ResourceQuota {
	m := map[string]domain.ResourceQuota{}
	for i := 0; i < len(pairs); i += 4 {
		m[pairs[i].(string)] = domain.ResourceQuota{
			VCPU:   pairs[i+1].(int),
			MemMB:  pairs[i+2].(int),
			DiskGB: pairs[i+3].(int),
		}
	}
	return m
}

func TestCheckAllocationConservedPool(t *testing.T) {
	b := Budget{TotalVCPU: 16, TotalMemMB: 24576, TotalDiskGB: 1000} // the kvm ceiling
	users := []*domain.User{
		{ID: "admin", Quotas: grants("kvm", 8, 12288, 400)},
		{ID: "alice", Quotas: grants("kvm", 4, 4096, 200)},
	}

	// Raising alice to fill the remaining headroom exactly is allowed (8 + 8 = 16).
	if err := b.CheckAllocation(users, "alice", "kvm", 8, 12288, 600); err != nil {
		t.Fatalf("allocation up to the total should be allowed: %v", err)
	}
	// One vCPU past the total is rejected (8 + 9 > 16).
	if err := b.CheckAllocation(users, "alice", "kvm", 9, 4096, 200); err == nil {
		t.Fatal("over-allocating vCPU past the platform total should be rejected")
	}
	// Memory past the total is rejected.
	if err := b.CheckAllocation(users, "alice", "kvm", 4, 20000, 200); err == nil {
		t.Fatal("over-allocating memory past the platform total should be rejected")
	}
	// Disk past the total is rejected - the same conserved pool, on the third dimension.
	if err := b.CheckAllocation(users, "alice", "kvm", 4, 4096, 700); err == nil {
		t.Fatal("over-allocating disk past the platform total should be rejected")
	}
	// A brand-new user (id not present) is treated as currently 0; 8 + 4 + 4 = 16 fits.
	if err := b.CheckAllocation(users, "bob", "kvm", 4, 4096, 100); err != nil {
		t.Fatalf("granting a new user within remaining headroom should be allowed: %v", err)
	}
	// Negative allocation is rejected, on any dimension.
	if err := b.CheckAllocation(users, "alice", "kvm", -1, 0, 0); err == nil {
		t.Fatal("negative allocation should be rejected")
	}
	if err := b.CheckAllocation(users, "alice", "kvm", 0, 0, -1); err == nil {
		t.Fatal("negative disk allocation should be rejected")
	}
}

// Each infrastructure's pool is conserved on its own: what's been granted on KVM must not consume
// any of vSphere's ceiling. Capacity is not fungible between backends.
func TestCheckAllocationIsPerProvider(t *testing.T) {
	vsphere := Budget{TotalVCPU: 64, TotalMemMB: 131072, TotalDiskGB: 4096}
	users := []*domain.User{
		{ID: "alice", Quotas: grants("kvm", 16, 24576, 500)}, // the whole kvm host, nothing on vsphere
	}
	if err := vsphere.CheckAllocation(users, "alice", "vsphere", 64, 131072, 4096); err != nil {
		t.Fatalf("alice's kvm grant must not consume the vsphere pool: %v", err)
	}
	// And her vSphere grant is bounded by vSphere's ceiling alone.
	if err := vsphere.CheckAllocation(users, "alice", "vsphere", 65, 0, 0); err == nil {
		t.Fatal("a grant past the vsphere ceiling = nil error, want a rejection")
	}
	// Disk is per-backend too: a KVM disk grant is not vSphere datastore capacity.
	if err := vsphere.CheckAllocation(users, "alice", "vsphere", 0, 0, 4097); err == nil {
		t.Fatal("a disk grant past the vsphere ceiling = nil error, want a rejection")
	}
}

func TestAllocated(t *testing.T) {
	users := []*domain.User{
		{Quotas: grants("kvm", 8, 100, 50, "vsphere", 32, 999, 800)},
		{Quotas: grants("kvm", 4, 200, 70)},
	}
	cpu, mem, disk := Allocated(users, "kvm")
	if cpu != 12 || mem != 300 || disk != 120 {
		t.Fatalf("Allocated(kvm) = %d/%d/%d, want 12/300/120", cpu, mem, disk)
	}
	cpu, mem, disk = Allocated(users, "vsphere")
	if cpu != 32 || mem != 999 || disk != 800 {
		t.Fatalf("Allocated(vsphere) = %d/%d/%d, want 32/999/800 - only the vsphere grants count", cpu, mem, disk)
	}
}

func TestAllocatedExcludesAdmins(t *testing.T) {
	// An admin's grant (however it got set - e.g. a leftover from before the auto-pool model) must
	// never count toward the conserved-pool sum: admins hold no fixed slice.
	users := []*domain.User{
		{IsAdmin: true, Quotas: grants("kvm", 999, 999999, 99999)},
		{Quotas: grants("kvm", 4, 4096, 80)},
	}
	cpu, mem, disk := Allocated(users, "kvm")
	if cpu != 4 || mem != 4096 || disk != 80 {
		t.Fatalf("Allocated (admin excluded) = %d/%d/%d, want 4/4096/80", cpu, mem, disk)
	}
}

func TestUnallocated(t *testing.T) {
	b := Budget{TotalVCPU: 16, TotalMemMB: 24576, TotalDiskGB: 1000}
	users := []*domain.User{
		{IsAdmin: true},
		{Quotas: grants("kvm", 4, 4096, 100)},
		{Quotas: grants("kvm", 6, 6144, 300)},
	}
	vcpu, mem, disk := b.Unallocated(users, "kvm")
	if vcpu != 6 || mem != 14336 || disk != 600 {
		t.Fatalf("Unallocated = %d/%d/%d, want 6/14336/600", vcpu, mem, disk)
	}
	// Floored at 0 even if somehow over-allocated (shouldn't happen via CheckAllocation, but stay
	// defensive).
	over := []*domain.User{{Quotas: grants("kvm", 999, 999999, 99999)}}
	vcpu, mem, disk = b.Unallocated(over, "kvm")
	if vcpu != 0 || mem != 0 || disk != 0 {
		t.Fatalf("Unallocated (over-allocated) = %d/%d/%d, want 0/0/0", vcpu, mem, disk)
	}
}

func TestPerUserAdmission(t *testing.T) {
	// A user's Budget is their personal quota on one infrastructure; admission counts only their
	// own clusters there.
	small := Budget{TotalVCPU: 4, TotalMemMB: 16384, TotalDiskGB: 1000} // two small control-plane-only clusters
	want := &domain.Cluster{Size: "small", ControlPlanes: 1}
	existing := []*domain.Cluster{
		{Size: "small", ControlPlanes: 1, Phase: domain.PhaseReady},
	}
	if err := small.Check(existing, want); err != nil {
		t.Fatalf("second small cluster should fit the personal quota: %v", err)
	}
	// A third would exceed it (3 * 2 vCPU > 4).
	existing = append(existing, &domain.Cluster{Size: "small", ControlPlanes: 1, Phase: domain.PhaseReady})
	if err := small.Check(existing, want); err == nil {
		t.Fatal("a cluster beyond the personal quota should be rejected")
	}
}

// A cluster is priced from its WHOLE shape: the control planes at the cluster's own size, and each
// pool's workers at that pool's. This is the invariant that keeps admission honest - a mixed-size
// cluster that passes quota must be exactly the cluster that gets provisioned.
func TestClusterUsageIsPerPool(t *testing.T) {
	c := &domain.Cluster{
		Size:          "small", // control plane: 2 vCPU / 8192 MB / 50 GB
		ControlPlanes: 1,
		NodePools: []domain.NodePool{
			{Name: "default", Size: "small", DesiredWorkers: 2}, // 2 × (2 / 8192 / 50)
			{Name: "big", Size: "large", DesiredWorkers: 1},     // 1 × (8 / 32768 / 50)
		},
	}
	vcpu, mem, disk := ClusterUsage(c)
	wantCPU := 2 + 2*2 + 8
	wantMem := 8192 + 2*8192 + 32768
	wantDisk := 50 + 2*50 + 50
	if vcpu != wantCPU || mem != wantMem || disk != wantDisk {
		t.Fatalf("ClusterUsage = %d/%d/%d, want %d/%d/%d - pools must be priced at their OWN size, not the cluster's",
			vcpu, mem, disk, wantCPU, wantMem, wantDisk)
	}
}

// A pool-less cluster costs only its control planes: pools are workers, and a cluster may have none.
func TestClusterUsageNoPools(t *testing.T) {
	vcpu, mem, disk := ClusterUsage(&domain.Cluster{Size: "medium", ControlPlanes: 3})
	if vcpu != 12 || mem != 3*16384 || disk != 3*50 {
		t.Fatalf("ClusterUsage(no pools) = %d/%d/%d, want 12/%d/%d", vcpu, mem, disk, 3*16384, 3*50)
	}
}

// A pool's root-disk override is what its workers actually cost on the hypervisor's storage pool, so
// admission must price it. Otherwise a pool of 500 GB workers would be admitted against the 50 GB
// default and quietly overrun the host's disk.
func TestClusterUsagePricesPoolRootDiskOverride(t *testing.T) {
	c := &domain.Cluster{
		Size:          "small", // control plane: 50 GB, unaffected by any pool override
		ControlPlanes: 1,
		NodePools: []domain.NodePool{
			{Name: "data", Size: "small", DesiredWorkers: 2, DiskGB: 500},
		},
	}
	_, _, disk := ClusterUsage(c)
	if want := 50 + 2*500; disk != want {
		t.Fatalf("ClusterUsage disk = %d, want %d - the pool's root-disk override is what its workers cost", disk, want)
	}
}

// Extra disks are real storage on the host and must be charged, whatever their phase - including
// "removing", whose volume still exists until the reconciler has actually destroyed it.
func TestClusterUsagePricesExtraDisks(t *testing.T) {
	c := &domain.Cluster{
		Size:          "small",
		ControlPlanes: 1,
		NodePools:     []domain.NodePool{{Name: "default", Size: "small", DesiredWorkers: 1}},
		NodeDisks: []domain.NodeDisk{
			{VMName: "c-default-0", Name: "data", SizeGB: 100, Phase: domain.DiskPhaseAttached},
			{VMName: "c-default-0", Name: "logs", SizeGB: 50, Phase: domain.DiskPhasePending},
			{VMName: "c-default-0", Name: "old", SizeGB: 25, Phase: domain.DiskPhaseRemoving},
		},
	}
	_, _, disk := ClusterUsage(c)
	if want := 50 + 50 + 100 + 50 + 25; disk != want {
		t.Fatalf("ClusterUsage disk = %d, want %d - extra disks are charged in every phase", disk, want)
	}
}

// Admission must price the candidate's pools, not just its node count - otherwise a large pool would
// be admitted against a small pool's budget.
func TestCheckPricesCandidatePools(t *testing.T) {
	b := Budget{TotalVCPU: 6, TotalMemMB: 1 << 30, TotalDiskGB: 1 << 20}
	cp := &domain.Cluster{Size: "small", ControlPlanes: 1} // 2 vCPU

	fits := *cp
	fits.NodePools = []domain.NodePool{{Name: "default", Size: "small", DesiredWorkers: 2}} // +4 = 6
	if err := b.Check(nil, &fits); err != nil {
		t.Fatalf("2 small workers should fit 6 vCPU: %v", err)
	}

	over := *cp
	over.NodePools = []domain.NodePool{{Name: "default", Size: "large", DesiredWorkers: 1}} // +8 = 10
	if err := b.Check(nil, &over); err == nil {
		t.Fatal("one LARGE worker (8 vCPU) must not pass a 6 vCPU budget - the pool's size is what costs")
	}
}

// Disk is a real admission gate, not just a reported figure: a cluster that fits in cores and memory
// is still rejected when its storage doesn't fit.
func TestCheckRejectsOnDiskAlone(t *testing.T) {
	b := Budget{TotalVCPU: 1 << 10, TotalMemMB: 1 << 30, TotalDiskGB: 100}
	want := &domain.Cluster{
		Size:          "small", // 50 GB root
		ControlPlanes: 1,
		NodePools:     []domain.NodePool{{Name: "default", Size: "small", DesiredWorkers: 1}}, // +40 = 80
		NodeDisks: []domain.NodeDisk{
			{VMName: "c-default-0", Name: "data", SizeGB: 50, Phase: domain.DiskPhasePending}, // = 130 > 100
		},
	}
	if err := b.Check(nil, want); err == nil {
		t.Fatal("a cluster that fits in vCPU and memory but not disk must still be rejected")
	}
}
