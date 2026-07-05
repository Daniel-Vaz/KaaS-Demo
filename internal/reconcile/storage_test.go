package reconcile

import (
	"context"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// storageCfg records the Longhorn disk calls, in order, alongside the plain disk ones - the
// ordering between the two is what makes removal non-destructive.
type storageCfg struct {
	*config.Fake
	calls    []string // "evict:<disk>" / "release:<disk>" / "register"
	register int
}

func (c *storageCfg) EnsureLonghornDisks(_ context.Context, _ *domain.Cluster) error {
	c.calls = append(c.calls, "register")
	c.register++
	return nil
}

func (c *storageCfg) EvictLonghornDisks(_ context.Context, _ *domain.Cluster, disks []domain.NodeDisk) error {
	for _, d := range disks {
		c.calls = append(c.calls, "evict:"+d.Name)
	}
	return nil
}

func (c *storageCfg) RemoveNodeDisks(_ context.Context, _ *domain.Cluster, disks []domain.NodeDisk) error {
	for _, d := range disks {
		c.calls = append(c.calls, "release:"+d.Name)
	}
	return nil
}

// storageCluster is a one-worker cluster with longhorn installed and its platform storage disk.
func storageCluster(id string) *domain.Cluster {
	return &domain.Cluster{
		ID: id, Name: "demo", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1, StorageDiskGB: 10,
		NodePools: []domain.NodePool{{Name: "default", Size: "small", DesiredWorkers: 1}},
		Addons:    []domain.Addon{{Name: "longhorn", Version: "1.12.0", Phase: "pending"}},
		NodeDisks: []domain.NodeDisk{{
			VMName: "demo-default-0", Name: domain.PlatformStorageDiskName, SizeGB: 10,
			MountPath: domain.LonghornDataPath, FSType: domain.FSExt4, Phase: domain.DiskPhasePending,
			WWN: domain.NewDiskWWN(id, "demo-default-0", domain.PlatformStorageDiskName),
		}},
	}
}

// The platform's own disk sits at Longhorn's default data path, so longhorn-manager finds it
// unprompted. Registering it again is an ERROR (Longhorn refuses two disks sharing a path), so the
// common cluster must do no wiring at all.
func TestPlatformStorageDiskIsNotRegistered(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &storageCfg{Fake: config.NewFake()}
	r.Cfg = cfg

	_ = st.CreateCluster(storageCluster("s1"))
	converge(t, r, st, "s1")

	if cfg.register != 0 {
		t.Fatalf("registered %d time(s), want none - the platform disk registers itself", cfg.register)
	}
	c, _ := st.GetCluster("s1")
	if c.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready", c.Phase)
	}
	if c.NodeDisks[0].Phase != domain.DiskPhaseAttached {
		t.Fatalf("the storage disk must be mounted during BRING-UP, before Longhorn installs; phase = %q",
			c.NodeDisks[0].Phase)
	}
}

// An EXTRA disk does need registering - and the marker is a fingerprint, so a disk attached to a
// long-Ready cluster still reaches Longhorn rather than being latched out.
func TestExtraStorageDiskIsRegisteredWhenAdded(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &storageCfg{Fake: config.NewFake()}
	r.Cfg = cfg

	_ = st.CreateCluster(storageCluster("s2"))
	converge(t, r, st, "s2")
	if cfg.register != 0 {
		t.Fatalf("registered %d time(s) before any extra disk", cfg.register)
	}

	got, _ := st.GetCluster("s2")
	got.NodeDisks = append(got.NodeDisks, domain.NodeDisk{
		VMName: "demo-default-0", Name: "extra", SizeGB: 20,
		MountPath: domain.LonghornMountPath("extra"), FSType: domain.FSExt4,
		Phase: domain.DiskPhasePending, WWN: domain.NewDiskWWN("s2", "demo-default-0", "extra"),
	})
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "s2")

	if cfg.register != 1 {
		t.Fatalf("registered %d time(s), want exactly one pass for the added disk", cfg.register)
	}
	after, _ := st.GetCluster("s2")
	if after.StorageWired != domain.StorageFingerprint(after) {
		t.Fatalf("storage_wired = %q, want the current disk set's fingerprint", after.StorageWired)
	}

	// An unrelated edit must NOT re-run it: the fingerprint is unchanged.
	after.Generation++
	_ = st.UpdateCluster(after)
	converge(t, r, st, "s2")
	if cfg.register != 1 {
		t.Fatalf("registered %d time(s), want the marker to skip an unrelated update", cfg.register)
	}
}

// THE ordering assertion. A registered disk holds volume replicas, so Longhorn must be asked to move
// them off BEFORE the guest unmounts the filesystem - unmounting first degrades every volume that
// had a replica there, and loses any whose only replica did.
func TestLonghornEvictionPrecedesUnmount(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &storageCfg{Fake: config.NewFake()}
	r.Cfg = cfg

	c := storageCluster("s3")
	c.NodeDisks = append(c.NodeDisks, domain.NodeDisk{
		VMName: "demo-default-0", Name: "extra", SizeGB: 20,
		MountPath: domain.LonghornMountPath("extra"), FSType: domain.FSExt4,
		Phase: domain.DiskPhasePending, WWN: domain.NewDiskWWN("s3", "demo-default-0", "extra"),
	})
	_ = st.CreateCluster(c)
	converge(t, r, st, "s3")
	cfg.calls = nil

	got, _ := st.GetCluster("s3")
	for i := range got.NodeDisks {
		if got.NodeDisks[i].Name == "extra" {
			got.NodeDisks[i].Phase = domain.DiskPhaseRemoving
		}
	}
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "s3")

	var evictAt, releaseAt = -1, -1
	for i, call := range cfg.calls {
		switch call {
		case "evict:extra":
			evictAt = i
		case "release:extra":
			releaseAt = i
		}
	}
	if evictAt < 0 || releaseAt < 0 {
		t.Fatalf("calls = %v, want both an eviction and a guest release", cfg.calls)
	}
	if evictAt > releaseAt {
		t.Fatalf("calls = %v - the disk was unmounted before Longhorn evicted its replicas", cfg.calls)
	}
	after, _ := st.GetCluster("s3")
	if len(after.NodeDisks) != 1 {
		t.Fatalf("NodeDisks = %+v, want only the platform disk left", after.NodeDisks)
	}
	// The fingerprint must have followed the removal, or a disk added later would find it unchanged
	// from before and never be registered.
	if after.StorageWired != domain.StorageFingerprint(after) {
		t.Fatalf("storage_wired = %q, want it back in step with the remaining disks", after.StorageWired)
	}
}

// A cluster that deselected longhorn keeps its disks as ordinary filesystems - there are no CRs to
// patch, which is a skip and not a failure.
func TestNoWiringWithoutTheLonghornAddon(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &storageCfg{Fake: config.NewFake()}
	r.Cfg = cfg

	c := storageCluster("s4")
	c.Addons = nil
	c.NodeDisks = append(c.NodeDisks, domain.NodeDisk{
		VMName: "demo-default-0", Name: "extra", SizeGB: 20,
		MountPath: domain.LonghornMountPath("extra"), FSType: domain.FSExt4,
		Phase: domain.DiskPhasePending, WWN: domain.NewDiskWWN("s4", "demo-default-0", "extra"),
	})
	_ = st.CreateCluster(c)
	converge(t, r, st, "s4")

	if cfg.register != 0 {
		t.Fatalf("registered %d time(s) on a cluster with no longhorn add-on", cfg.register)
	}
	got, _ := st.GetCluster("s4")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want the cluster to reach Ready regardless", got.Phase)
	}
}
