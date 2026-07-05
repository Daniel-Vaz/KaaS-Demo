// Package quota guards against oversubscribing an infrastructure - a first-class feature, not an
// afterthought (VMs oversubscribe a laptop fast). See docs/architecture.md.
//
// Everything here is scoped to ONE infrastructure at a time: each provider has its own ceiling
// (the KVM host's cores, the vSphere cluster's budget), each user holds a separate grant on each,
// and the grants on a provider sum to at most that provider's ceiling. Capacity is not fungible
// between backends, so a single cross-provider pool would admit clusters against capacity that
// cannot physically host them.
//
// Capacity has THREE dimensions - vCPU, memory and disk. Disk is metered for the same reason as the
// other two: it is finite on the hypervisor's storage pool, and since node pools carry a root-disk
// override and nodes carry extra disks (domain.NodeDisk), it is a dimension a tenant can spend
// directly and without bound. A cluster that fits in cores and RAM can still fill the host's disk.
package quota

import (
	"fmt"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Budget is the host capacity the platform is allowed to hand out.
type Budget struct {
	TotalVCPU   int
	TotalMemMB  int
	TotalDiskGB int
}

// ClusterUsage returns the vCPU, MemMB and DiskGB a cluster's VMs occupy. Nodes are NOT sized alike
// since node pools landed: the control planes take the cluster's own t-shirt size, and each pool's
// workers take that pool's (including its root-disk override). So this walks the cluster's desired
// node set (domain.DesiredNodes) - the same function the reconciler builds real VMs from - rather
// than multiplying one size by a node count. Deriving both from one place is what keeps admission
// honest: a shape that passes quota is exactly the shape that gets provisioned.
//
// Disk is the root disks PLUS every extra NodeDisk. Extra disks are counted whatever their phase,
// including "removing": the volume still exists on the hypervisor until the reconciler has actually
// detached and destroyed it, and releasing the charge the moment a user asks for it back would let
// them re-spend capacity the platform has not reclaimed yet.
func ClusterUsage(c *domain.Cluster) (vcpu, memMB, diskGB int) {
	for _, n := range domain.DesiredNodes(c) {
		vcpu += n.Spec.CPUs
		memMB += n.Spec.MemMB
		diskGB += n.Spec.DiskGB
	}
	for _, d := range c.NodeDisks {
		diskGB += d.SizeGB
	}
	return vcpu, memMB, diskGB
}

// Usage sums the vCPU, memory and disk currently allocated by all live (non-terminal, not
// deleting) clusters. Used both for admission and for surfacing headroom in the UI.
func Usage(existing []*domain.Cluster) (vcpu, memMB, diskGB int) {
	for _, c := range existing {
		if c.Phase.Terminal() || c.Phase == domain.PhaseDeleting {
			continue
		}
		cpu, mem, disk := ClusterUsage(c)
		vcpu += cpu
		memMB += mem
		diskGB += disk
	}
	return vcpu, memMB, diskGB
}

// Allocated sums the quota handed out ON ONE INFRASTRUCTURE across all NON-ADMIN users. Admins
// don't hold a fixed slice - their budget is whatever's left over (see Unallocated) - so they're
// excluded here; this is what makes granting a tenant not require first shrinking the admin.
//
// The conserved pool is per-provider because capacity is: the KVM host's cores and the vSphere
// cluster's cores are different physical machines, and a grant on one can never be spent on the
// other.
func Allocated(users []*domain.User, provider string) (vcpu, memMB, diskGB int) {
	for _, u := range users {
		if u.IsAdmin {
			continue
		}
		q := u.QuotaOn(provider)
		vcpu += q.VCPU
		memMB += q.MemMB
		diskGB += q.DiskGB
	}
	return vcpu, memMB, diskGB
}

// Unallocated returns one infrastructure's capacity not yet granted to any non-admin account -
// what an admin can consume directly there (their own clusters draw from it) and what's still
// available to grant. Floored at 0 (over-allocation is prevented by CheckAllocation, but this
// stays defensive).
func (b Budget) Unallocated(users []*domain.User, provider string) (vcpu, memMB, diskGB int) {
	allocCPU, allocMem, allocDisk := Allocated(users, provider)
	vcpu, memMB, diskGB = b.TotalVCPU-allocCPU, b.TotalMemMB-allocMem, b.TotalDiskGB-allocDisk
	if vcpu < 0 {
		vcpu = 0
	}
	if memMB < 0 {
		memMB = 0
	}
	if diskGB < 0 {
		diskGB = 0
	}
	return vcpu, memMB, diskGB
}

// CheckAllocation verifies that setting non-admin user userID's grant ON provider to
// (vcpu, memMB, diskGB) keeps the sum of all non-admin users' grants on that infrastructure within
// its budget - the conserved-pool rule, enforced per infrastructure, which is what guarantees real
// VM usage can never oversubscribe a host (every user stays within their grant, and the grants on
// each backend sum to at most that backend's physical total). b must be THAT provider's budget.
// userID may be a not-yet-created user (its current allocation is 0). Callers must not invoke this
// for an admin target - admins don't hold a slice (see Unallocated); the app layer rejects that
// first.
func (b Budget) CheckAllocation(users []*domain.User, userID, provider string, vcpu, memMB, diskGB int) error {
	if vcpu < 0 || memMB < 0 || diskGB < 0 {
		return fmt.Errorf("quota: allocation must be non-negative")
	}
	allocCPU, allocMem, allocDisk := Allocated(users, provider)
	for _, u := range users { // subtract the user's current allocation - we're replacing it
		if u.ID == userID && !u.IsAdmin {
			q := u.QuotaOn(provider)
			allocCPU -= q.VCPU
			allocMem -= q.MemMB
			allocDisk -= q.DiskGB
			break
		}
	}
	if allocCPU+vcpu > b.TotalVCPU {
		return fmt.Errorf("quota: %s vCPU allocation exceeds that infrastructure's total (%d + %d > %d)", provider, allocCPU, vcpu, b.TotalVCPU)
	}
	if allocMem+memMB > b.TotalMemMB {
		return fmt.Errorf("quota: %s memory allocation exceeds that infrastructure's total (%d + %d MB > %d MB)", provider, allocMem, memMB, b.TotalMemMB)
	}
	if allocDisk+diskGB > b.TotalDiskGB {
		return fmt.Errorf("quota: %s disk allocation exceeds that infrastructure's total (%d + %d GB > %d GB)", provider, allocDisk, diskGB, b.TotalDiskGB)
	}
	return nil
}

// Check verifies that admitting `want` keeps total allocation within budget, given the already-live
// clusters. Terminal clusters don't count. In the multi-tenant model this is called with the owner's
// quota as the Budget and the owner's own clusters as existing, so admission charges each user's
// personal quota.
//
// `want` is the CANDIDATE cluster - its full desired shape: control-plane size, node pools (each
// with its root-disk override) and extra disks. It need not exist yet (a create passes the struct it
// is about to persist) and it must not appear in `existing` (an edit passes the shape it wants
// alongside the owner's OTHER clusters, so the cluster isn't charged twice - see clustersExcept in
// internal/app).
func (b Budget) Check(existing []*domain.Cluster, want *domain.Cluster) error {
	usedCPU, usedMem, usedDisk := Usage(existing)
	wantCPU, wantMem, wantDisk := ClusterUsage(want)
	if usedCPU+wantCPU > b.TotalVCPU {
		return fmt.Errorf("quota: vCPU budget exceeded (%d + %d > %d)", usedCPU, wantCPU, b.TotalVCPU)
	}
	if usedMem+wantMem > b.TotalMemMB {
		return fmt.Errorf("quota: memory budget exceeded (%d + %d MB > %d MB)", usedMem, wantMem, b.TotalMemMB)
	}
	if usedDisk+wantDisk > b.TotalDiskGB {
		return fmt.Errorf("quota: disk budget exceeded (%d + %d GB > %d GB)", usedDisk, wantDisk, b.TotalDiskGB)
	}
	return nil
}
