package app

import (
	"strconv"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// storageDisks is a cluster's platform-provisioned storage disks, by VM name.
func storageDisks(c *domain.Cluster) map[string]domain.NodeDisk {
	out := map[string]domain.NodeDisk{}
	for _, d := range c.NodeDisks {
		if d.IsPlatformStorage() {
			out[d.VMName] = d
		}
	}
	return out
}

// Every cluster is born with one storage disk per WORKER, mounted where Longhorn expects its data -
// which is the whole mechanism by which a plain PVC gets a real volume.
func TestCreateProvisionsAStorageDiskPerWorker(t *testing.T) {
	a, owner := newPoolApp(t)
	c, err := a.CreateCluster(owner, CreateRequest{
		Name: "dev", Size: "small",
		NodePools: []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	disks := storageDisks(c)
	if len(disks) != 2 {
		t.Fatalf("storage disks = %+v, want one per worker", c.NodeDisks)
	}
	for _, vm := range []string{"dev-default-0", "dev-default-1"} {
		d, ok := disks[vm]
		if !ok {
			t.Fatalf("no storage disk for %s", vm)
		}
		if d.MountPath != domain.LonghornDataPath {
			t.Errorf("%s mount path = %q, want Longhorn's data path %q - otherwise the cluster's storage silently lands on the root disk",
				vm, d.MountPath, domain.LonghornDataPath)
		}
		if d.SizeGB != domain.DefaultStorageDiskGB {
			t.Errorf("%s size = %d, want the %d GB default", vm, d.SizeGB, domain.DefaultStorageDiskGB)
		}
		if d.WWN == "" {
			t.Errorf("%s has no WWN - Ansible resolves the device by it", vm)
		}
	}
	// Control planes get none: a control plane's storage is the platform's business (etcd lives
	// there), and Longhorn's manager doesn't run on them anyway.
	if _, ok := disks["dev-cp-0"]; ok {
		t.Error("a control plane must not get a storage disk")
	}
	if c.StorageDiskGB != domain.DefaultStorageDiskGB {
		t.Errorf("storage_disk_gb = %d, want it recorded on the row", c.StorageDiskGB)
	}
}

// A cluster without the longhorn add-on has nothing to back, so it is not charged for disks it
// would never use.
func TestNoLonghornMeansNoStorageDisks(t *testing.T) {
	a, owner := newPoolApp(t)
	// longhorn is a bundle add-on, so dropping it at create time needs the lock lifted.
	a.BundleAddonsOptional = true
	c, err := a.CreateCluster(owner, CreateRequest{
		Name: "dev", Size: "small", Addons: []string{"metrics-server"},
		NodePools: []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.NodeDisks) != 0 || c.StorageDiskGB != 0 {
		t.Fatalf("NodeDisks = %+v / storage_disk_gb = %d, want none without the longhorn add-on",
			c.NodeDisks, c.StorageDiskGB)
	}
}

// An explicit 0 is a real choice ("provision no storage disks"), distinct from omitting the field.
func TestStorageDiskSizeIsHonoured(t *testing.T) {
	a, owner := newPoolApp(t)
	size := 25
	c, err := a.CreateCluster(owner, CreateRequest{
		Name: "sized", Size: "small", StorageDiskGB: &size,
		NodePools: []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := storageDisks(c)["sized-default-0"]; d.SizeGB != size {
		t.Fatalf("size = %d, want the requested %d", d.SizeGB, size)
	}

	zero := 0
	none, err := a.CreateCluster(owner, CreateRequest{
		Name: "none", Size: "small", StorageDiskGB: &zero,
		NodePools: []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.NodeDisks) != 0 {
		t.Fatalf("NodeDisks = %+v, want an explicit 0 to mean none", none.NodeDisks)
	}

	over := domain.MaxDiskGB + 1
	if _, err := a.CreateCluster(owner, CreateRequest{Name: "huge", Size: "small", StorageDiskGB: &over}); err == nil {
		t.Fatal("an out-of-range storage disk size should be rejected")
	}
}

// Scaling a pool up must give the NEW workers the same storage as their siblings - otherwise a
// scaled cluster has nodes Longhorn can place nothing on, and its capacity silently stops growing.
func TestScalingUpGivesNewWorkersStorage(t *testing.T) {
	a, owner := newPoolApp(t)
	c, err := a.CreateCluster(owner, CreateRequest{
		Name: "dev", Size: "small",
		NodePools: []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.UpdateCluster(owner, c.ID, UpdateRequest{
		NodePools: &[]domain.NodePool{
			{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: 2},
			{Name: "gpu", Size: "small", DesiredWorkers: 1},
		},
	})
	if err != nil {
		t.Fatalf("scale up: %v", err)
	}
	disks := storageDisks(got)
	for _, vm := range []string{"dev-default-0", "dev-default-1", "dev-gpu-0"} {
		if _, ok := disks[vm]; !ok {
			t.Errorf("no storage disk for %s after the scale-up", vm)
		}
	}
	if len(disks) != 3 {
		t.Fatalf("storage disks = %+v, want exactly one per worker", got.NodeDisks)
	}
}

// The platform's disk is derived from the cluster's storage size, so it cannot be deleted on its
// own - the next admission would simply mint it back, having left the node without the storage its
// replicas live on in between.
func TestPlatformStorageDiskCannotBeRemoved(t *testing.T) {
	a, owner := newPoolApp(t)
	c := readyDiskCluster(t, a, owner, "dev")
	if _, err := a.RemoveNodeDisk(owner, c.ID, "dev-default-0", domain.PlatformStorageDiskName); err == nil {
		t.Fatal("removing the platform storage disk should be rejected")
	}
	// ...and a user cannot mint one by that name either, which would be indistinguishable from it.
	if _, err := a.AddNodeDisk(owner, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: domain.PlatformStorageDiskName, SizeGB: 10,
	}); err == nil {
		t.Fatal("a user disk by the reserved name should be rejected")
	}
}

// A user's extra disk defaults to feeding the storage pool - which means being mounted under the
// Longhorn data path, since that IS the registration mechanism.
func TestAddedDiskDefaultsToTheStoragePool(t *testing.T) {
	a, owner := newPoolApp(t)
	c := readyDiskCluster(t, a, owner, "dev")
	got := addDisk(t, a, owner, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "extra", SizeGB: 20,
	})
	var d domain.NodeDisk
	for _, x := range got.NodeDisks {
		if x.Name == "extra" {
			d = x
		}
	}
	if want := domain.LonghornMountPath("extra"); d.MountPath != want {
		t.Fatalf("mount path = %q, want %q", d.MountPath, want)
	}
	if !d.FeedsStoragePool() {
		t.Fatal("a disk added with no mount path must feed the storage pool")
	}
	// An explicit path elsewhere stays an ordinary filesystem - the escape hatch.
	got = addDisk(t, a, owner, c.ID, AddNodeDiskRequest{
		VMName: "dev-default-0", Name: "plain", SizeGB: 5, MountPath: "/var/lib/plain",
	})
	for _, x := range got.NodeDisks {
		if x.Name == "plain" && x.FeedsStoragePool() {
			t.Fatal("a disk mounted outside the Longhorn data path must not feed the pool")
		}
	}
}

// The replica factor follows the cluster's own worker count: the chart's 3 would leave every volume
// on a two-worker cluster permanently degraded, and a flat 1 gives up surviving a lost node.
func TestLonghornReplicasFollowWorkerCount(t *testing.T) {
	extras := longhornAddonExtras()
	addon := domain.Addon{Name: longhornAddon}
	for _, tc := range []struct{ workers, want int }{{0, 1}, {1, 1}, {2, 2}, {3, 3}, {7, 3}} {
		c := &domain.Cluster{NodePools: []domain.NodePool{{Name: "default", DesiredWorkers: tc.workers}}}
		got := extras(c, addon).Values["defaultSettings.defaultReplicaCount"]
		if want := strconv.Itoa(tc.want); got != want {
			t.Errorf("%d workers: replicas = %q, want %q", tc.workers, got, want)
		}
		// Both keys must agree, or the StorageClass and the global default disagree.
		if got != extras(c, addon).Values["persistence.defaultClassReplicaCount"] {
			t.Errorf("%d workers: the two replica keys disagree", tc.workers)
		}
	}
	// A user who edited the add-on's values owns them - unlike external-dns, there is nothing here
	// the platform has to defend.
	overridden := domain.Addon{Name: longhornAddon, ValuesOverride: "defaultSettings: {}"}
	c := &domain.Cluster{NodePools: []domain.NodePool{{Name: "default", DesiredWorkers: 2}}}
	if len(extras(c, overridden).Values) != 0 {
		t.Error("a values override must stand the platform's replica default down")
	}
	// And it configures nothing else.
	if len(extras(c, domain.Addon{Name: "metrics-server"}).Values) != 0 {
		t.Error("longhornAddonExtras must only configure longhorn")
	}
}
