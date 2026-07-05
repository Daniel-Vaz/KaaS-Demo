package app

import (
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// readyDiskCluster creates a two-worker cluster and forces it Ready (no reconciler runs in these
// tests), which is the state disk edits require.
func readyDiskCluster(t *testing.T, a *App, owner *domain.User, name string) *domain.Cluster {
	t.Helper()
	c, err := a.CreateCluster(owner, CreateRequest{
		Name: name, Size: "small",
		NodePools: []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 2}},
	})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	c.Phase = domain.PhaseReady
	if err := a.Store.UpdateCluster(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// userDisks is a cluster's disks minus the platform's own per-worker storage disk, which every
// cluster is now born with (domain.DesiredStorageDisks). These tests are about the disks a USER
// attaches, so they assert on this rather than on the whole list.
func userDisks(c *domain.Cluster) []domain.NodeDisk {
	var out []domain.NodeDisk
	for _, d := range c.NodeDisks {
		if !d.IsPlatformStorage() {
			out = append(out, d)
		}
	}
	return out
}

func addDisk(t *testing.T, a *App, actor *domain.User, id string, req AddNodeDiskRequest) *domain.Cluster {
	t.Helper()
	c, err := a.AddNodeDisk(actor, id, req)
	if err != nil {
		t.Fatalf("add disk: %v", err)
	}
	return c
}

func TestAddNodeDisk(t *testing.T) {
	a, owner := newPoolApp(t)
	c := readyDiskCluster(t, a, owner, "dev")

	got := addDisk(t, a, owner, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "data", SizeGB: 20, MountPath: "/var/lib/data",
	})
	if len(userDisks(got)) != 1 {
		t.Fatalf("NodeDisks = %+v, want one", userDisks(got))
	}
	d := userDisks(got)[0]
	if d.Phase != domain.DiskPhasePending {
		t.Errorf("phase = %q, want %q - the reconciler is what attaches it", d.Phase, domain.DiskPhasePending)
	}
	if d.FSType != domain.FSExt4 {
		t.Errorf("fs_type = %q, want the ext4 default", d.FSType)
	}
	if d.WWN == "" {
		t.Error("WWN must be minted at admission - it is the guest's handle on the device")
	}
	if got.Generation != c.Generation+1 {
		t.Errorf("generation = %d, want it bumped so the reconciler converges", got.Generation)
	}
}

// A disk is pinned to a node, so it can only be added once the nodes exist - and the reconciler only
// converges disks from the Updating path, which is reachable from Ready alone.
func TestAddNodeDiskRequiresReady(t *testing.T) {
	a, owner := newPoolApp(t)
	c, err := a.CreateCluster(owner, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddNodeDisk(owner, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
	}); err == nil {
		t.Fatal("adding a disk to a not-yet-Ready cluster should be rejected")
	}
}

func TestAddNodeDiskValidates(t *testing.T) {
	a, owner := newPoolApp(t)
	c := readyDiskCluster(t, a, owner, "dev")

	bad := []struct {
		name string
		req  AddNodeDiskRequest
	}{
		{"control plane", AddNodeDiskRequest{VMName: "dev-cp-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data"}},
		{"unknown node", AddNodeDiskRequest{VMName: "dev-default-9", Name: "data", SizeGB: 10, MountPath: "/var/lib/data"}},
		{"system mount", AddNodeDiskRequest{VMName: "dev-default-0", Name: "data", SizeGB: 10, MountPath: "/etc"}},
		{"bad name", AddNodeDiskRequest{VMName: "dev-default-0", Name: "Data", SizeGB: 10, MountPath: "/var/lib/data"}},
		{"zero size", AddNodeDiskRequest{VMName: "dev-default-0", Name: "data", SizeGB: 0, MountPath: "/var/lib/data"}},
	}
	for _, tc := range bad {
		if _, err := a.AddNodeDisk(owner, c.ID, tc.req); err == nil {
			t.Errorf("%s: should be rejected", tc.name)
		}
	}
	// A rejected add must leave no trace.
	fresh, _ := a.Store.GetCluster(c.ID)
	if len(userDisks(fresh)) != 0 {
		t.Fatalf("a rejected add left state behind: %+v", userDisks(fresh))
	}
}

func TestAddNodeDiskRejectsDuplicate(t *testing.T) {
	a, owner := newPoolApp(t)
	c := readyDiskCluster(t, a, owner, "dev")
	req := AddNodeDiskRequest{VMName: "dev-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data"}
	addDisk(t, a, owner, c.ID, req)
	req.MountPath = "/var/lib/other"
	if _, err := a.AddNodeDisk(owner, c.ID, req); err == nil {
		t.Fatal("a second disk with the same name on one node should be rejected")
	}
}

// Removal must NOT drop the row: the row is what keeps the volume attached while the reconciler
// unmounts the filesystem in the guest. Dropping it here would detach the device from under a live
// mount.
func TestRemoveNodeDiskMarksRemovingRatherThanDeleting(t *testing.T) {
	a, owner := newPoolApp(t)
	c := readyDiskCluster(t, a, owner, "dev")
	addDisk(t, a, owner, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
	})

	got, err := a.RemoveNodeDisk(owner, c.ID, "dev-default-0", "data")
	if err != nil {
		t.Fatalf("remove disk: %v", err)
	}
	if len(userDisks(got)) != 1 {
		t.Fatalf("the disk row must survive until the guest has released it, got %+v", userDisks(got))
	}
	if userDisks(got)[0].Phase != domain.DiskPhaseRemoving {
		t.Errorf("phase = %q, want %q", userDisks(got)[0].Phase, domain.DiskPhaseRemoving)
	}
}

func TestRemoveNodeDiskUnknown(t *testing.T) {
	a, owner := newPoolApp(t)
	c := readyDiskCluster(t, a, owner, "dev")
	if _, err := a.RemoveNodeDisk(owner, c.ID, "dev-default-0", "ghost"); err == nil {
		t.Fatal("removing a disk that doesn't exist should be rejected")
	}
}

// Disk capacity is charged to the owner's grant, like every other resource: a tenant cannot fill the
// host's storage pool one extra disk at a time.
//
// The fixture grants 4096 GB on kvm; the cluster's 3 × 40 GB of root disks leave 3976. So two
// max-size (2000 GB) disks overrun it, and the second must be turned away.
func TestAddNodeDiskChargesQuota(t *testing.T) {
	a, owner := newPoolApp(t)
	c := readyDiskCluster(t, a, owner, "dev")

	addDisk(t, a, owner, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "big1", SizeGB: domain.MaxDiskGB, MountPath: "/var/lib/big1",
	})
	if _, err := a.AddNodeDisk(owner, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "big2", SizeGB: domain.MaxDiskGB, MountPath: "/var/lib/big2",
	}); err == nil {
		t.Fatal("a disk that pushes the owner past their disk grant should be rejected")
	}
	// And the rejected disk left nothing behind.
	fresh, _ := a.Store.GetCluster(c.ID)
	if len(userDisks(fresh)) != 1 {
		t.Fatalf("NodeDisks = %+v, want only the admitted disk", userDisks(fresh))
	}
}

// Scaling a pool down un-desires its highest-numbered nodes; a disk pinned to one goes with it. A
// stale row would be desired state nothing converges - and would make every later disk edit on the
// cluster fail validation.
func TestScalingDownPrunesItsNodesDisks(t *testing.T) {
	a, owner := newPoolApp(t)
	c := readyDiskCluster(t, a, owner, "dev")
	addDisk(t, a, owner, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-1", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
	})

	got, err := a.UpdateCluster(owner, c.ID, UpdateRequest{
		NodePools: &[]domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 1}},
	})
	if err != nil {
		t.Fatalf("scale down: %v", err)
	}
	if len(userDisks(got)) != 0 {
		t.Fatalf("the removed node's disk should have been pruned, got %+v", userDisks(got))
	}
	// ...and the departing node's PLATFORM disk went with it, leaving exactly one worker's worth.
	if len(got.NodeDisks) != 1 || got.NodeDisks[0].VMName != "dev-default-0" {
		t.Fatalf("NodeDisks = %+v, want only the remaining worker's storage disk", got.NodeDisks)
	}
	// And the cluster must still accept disk edits afterwards.
	if _, err := a.AddNodeDisk(owner, got.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
	}); err != nil {
		t.Fatalf("cluster should still accept disks after a scale-down: %v", err)
	}
}

// A pool's root-disk override sizes its workers' boot disk and must be immutable: growing it
// re-creates each node's volume, which rebuilds every VM in the pool under a live kubelet.
func TestPoolDiskSizeIsImmutable(t *testing.T) {
	a, owner := newPoolApp(t)
	c, err := a.CreateCluster(owner, CreateRequest{
		Name: "dev", Size: "small",
		NodePools: []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 1, DiskGB: 100}},
	})
	if err != nil {
		t.Fatalf("create with a root-disk override: %v", err)
	}
	if got := c.NodePools[0].DiskGB; got != 100 {
		t.Fatalf("pool disk_gb = %d, want the requested 100", got)
	}
	if _, err := a.UpdateCluster(owner, c.ID, UpdateRequest{
		NodePools: &[]domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 1, DiskGB: 200}},
	}); err == nil {
		t.Fatal("changing a pool's disk size should be rejected - it would rebuild every node in it")
	}
}

// The override may only grow the root disk: a node's volume is a COW clone of the golden image, and
// a volume smaller than the image it clones cannot be created at all.
func TestPoolDiskSizeFloorAndCeiling(t *testing.T) {
	a, owner := newPoolApp(t)
	for _, tc := range []struct {
		name   string
		diskGB int
	}{
		{"below the size default", 20},
		{"past the ceiling", domain.MaxRootDiskGB + 1},
	} {
		if _, err := a.CreateCluster(owner, CreateRequest{
			Name: "c" + tc.name[:3], Size: "small",
			NodePools: []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DiskGB: tc.diskGB}},
		}); err == nil {
			t.Errorf("%s: disk_gb %d should be rejected", tc.name, tc.diskGB)
		}
	}
}

// A read-role group-mate may look but not touch: disks are a write operation, and removing one
// destroys data.
func TestNodeDiskWritesAreWriteScoped(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	bob, _ := a.Register("bob", "password")
	g, err := a.CreateGroup(admin(t, a), "team")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*domain.User{alice, bob} {
		if _, err := a.UpdateUser(admin(t, a), u.ID, UpdateUserRequest{
			Memberships: &[]domain.GroupMembership{{GroupID: g.ID, Role: domain.GroupRoleRead}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	alice = grantQuota(t, a, alice.ID, 8, 49152)
	bob, _ = a.Store.GetUser(bob.ID)

	c := readyDiskCluster(t, a, alice, "dev")
	if _, err := a.AddNodeDisk(bob, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
	}); err == nil {
		t.Fatal("a read-role group-mate must not add a disk to someone else's cluster")
	}
	addDisk(t, a, alice, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
	})
	if _, err := a.RemoveNodeDisk(bob, c.ID, "dev-default-0", "data"); err == nil {
		t.Fatal("a read-role group-mate must not remove a disk - that destroys data")
	}
}
