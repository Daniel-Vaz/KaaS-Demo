package reconcile

import (
	"context"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
)

// diskCfg records the disk calls the reconciler makes, in order, so the tests can assert on the
// SEQUENCE - which is where the safety of disk removal actually lives.
type diskCfg struct {
	*config.Fake
	calls    []string // "release:<disk>" / "mount"
	released []string
}

func (c *diskCfg) RemoveNodeDisks(_ context.Context, _ *domain.Cluster, disks []domain.NodeDisk) error {
	for _, d := range disks {
		c.calls = append(c.calls, "release:"+d.Name)
		c.released = append(c.released, d.VMName+"/"+d.Name)
	}
	return nil
}

func (c *diskCfg) EnsureNodeDisks(_ context.Context, _ *domain.Cluster) error {
	c.calls = append(c.calls, "mount")
	return nil
}

// diskProv records the disk specs each EnsureNodes call was asked for, so a test can see exactly
// when a volume stopped being desired (i.e. when it would be detached and destroyed).
type diskProv struct {
	*provision.Fake
	specs [][]string // per call: "<vm>/<disk>" for every desired disk
}

func (p *diskProv) EnsureNodes(ctx context.Context, clusterID string, net provision.NetworkSpec, specs []provision.NodeSpec) ([]provision.ProvisionedNode, error) {
	var seen []string
	for _, s := range specs {
		for _, d := range s.Disks {
			seen = append(seen, s.VMName+"/"+d.Name)
		}
	}
	p.specs = append(p.specs, seen)
	return p.Fake.EnsureNodes(ctx, clusterID, net, specs)
}

func diskTestCluster(id string) *domain.Cluster {
	return &domain.Cluster{
		ID: id, Name: "demo", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
		NodePools: []domain.NodePool{{Name: "default", Size: "small", DesiredWorkers: 1}},
	}
}

// A pending disk is created, attached, formatted and mounted, and ends up "attached" carrying the
// device identity the provisioner reported - which is what Ansible resolves the real device through.
func TestReconcileAttachesAndMountsDisk(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &diskCfg{Fake: config.NewFake()}
	r.Cfg = cfg

	c := diskTestCluster("d1")
	_ = st.CreateCluster(c)
	converge(t, r, st, "d1")

	got, _ := st.GetCluster("d1")
	got.NodeDisks = []domain.NodeDisk{{
		VMName: "demo-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
		FSType: domain.FSExt4, Phase: domain.DiskPhasePending,
		WWN: domain.NewDiskWWN("d1", "demo-default-0", "data"),
	}}
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "d1")

	after, _ := st.GetCluster("d1")
	if len(after.NodeDisks) != 1 {
		t.Fatalf("NodeDisks = %+v, want the disk to survive", after.NodeDisks)
	}
	d := after.NodeDisks[0]
	if d.Phase != domain.DiskPhaseAttached {
		t.Errorf("phase = %q, want %q once mounted", d.Phase, domain.DiskPhaseAttached)
	}
	if d.DeviceID == "" {
		t.Error("DeviceID must be stamped from what the provisioner observed - Ansible resolves the device by it")
	}
}

// The ordering that makes removal safe: the guest releases the disk (unmount, fstab, vgremove) while
// the volume is STILL attached, and only then does the volume stop being desired. Detaching first
// would leave a mount over a device that no longer exists.
func TestReconcileReleasesDiskBeforeDetaching(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &diskCfg{Fake: config.NewFake()}
	r.Cfg = cfg
	prov := &diskProv{Fake: provision.NewFake()}
	r.Prov = prov

	c := diskTestCluster("d2")
	_ = st.CreateCluster(c)
	converge(t, r, st, "d2")

	// Attach a disk and let it settle.
	got, _ := st.GetCluster("d2")
	got.NodeDisks = []domain.NodeDisk{{
		VMName: "demo-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
		FSType: domain.FSExt4, Phase: domain.DiskPhasePending,
		WWN: domain.NewDiskWWN("d2", "demo-default-0", "data"),
	}}
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "d2")

	callsBefore := len(prov.specs)
	// Now ask for it back, the way the API does: mark it removing, don't delete the row.
	got, _ = st.GetCluster("d2")
	got.NodeDisks[0].Phase = domain.DiskPhaseRemoving
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "d2")

	if len(cfg.released) != 1 || cfg.released[0] != "demo-default-0/data" {
		t.Fatalf("released = %v, want the guest to have released the disk", cfg.released)
	}
	after, _ := st.GetCluster("d2")
	if len(after.NodeDisks) != 0 {
		t.Fatalf("NodeDisks = %+v, want the row dropped once the guest released it", after.NodeDisks)
	}
	// THE assertion: every EnsureNodes call made after the removal was asked for must no longer
	// desire the disk - i.e. the detach happened strictly after the release above.
	if len(prov.specs) <= callsBefore {
		t.Fatal("expected the reconciler to re-converge infra after releasing the disk")
	}
	for _, seen := range prov.specs[callsBefore:] {
		if len(seen) != 0 {
			t.Fatalf("EnsureNodes still desired %v after the release - the volume would be detached "+
				"while the guest still had it mounted", seen)
		}
	}
	// And before the removal, the volume WAS desired - otherwise the assertion above proves nothing.
	if len(prov.specs[callsBefore-1]) == 0 {
		t.Fatal("the disk should have been desired while it was attached")
	}
}

// A disk still being removed must not be re-mounted: the platform would be tearing it down and the
// role re-adding it, every tick, forever.
func TestReconcileDoesNotRemountARemovingDisk(t *testing.T) {
	r, st := newTestReconciler(t)
	cfg := &diskCfg{Fake: config.NewFake()}
	r.Cfg = cfg

	c := diskTestCluster("d3")
	_ = st.CreateCluster(c)
	converge(t, r, st, "d3")
	// Bring-up mounts the disks a cluster is CREATED with (every cluster now gets a per-worker
	// storage disk), so start the recording from the removal itself - the ordering under test is
	// within that tick.
	cfg.calls = nil

	got, _ := st.GetCluster("d3")
	got.NodeDisks = []domain.NodeDisk{{
		VMName: "demo-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
		FSType: domain.FSExt4, Phase: domain.DiskPhaseRemoving,
		WWN: domain.NewDiskWWN("d3", "demo-default-0", "data"),
	}}
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "d3")

	// The release must come before any mount pass in the same tick.
	if len(cfg.calls) == 0 || cfg.calls[0] != "release:data" {
		t.Fatalf("calls = %v, want the release first", cfg.calls)
	}
	after, _ := st.GetCluster("d3")
	if len(after.NodeDisks) != 0 {
		t.Fatalf("NodeDisks = %+v, want the disk gone", after.NodeDisks)
	}
}

// lateIDProv reports no identity for a disk the FIRST time it is asked to create one, and reports it
// on every call after - vCenter's actual behaviour: it mints the VMDK's UUID and does not report it
// on the tick that creates the disk.
//
// Keyed on "the first call that carries a disk", not on a plain call count: EnsureNodes is called
// several times during bring-up, long before any disk exists, and a counter would burn its
// withholding on one of those and never exercise the case at all.
type lateIDProv struct {
	*provision.Fake
	withheld bool
}

func (p *lateIDProv) EnsureNodes(ctx context.Context, clusterID string, net provision.NetworkSpec, specs []provision.NodeSpec) ([]provision.ProvisionedNode, error) {
	nodes, err := p.Fake.EnsureNodes(ctx, clusterID, net, specs)
	if err != nil {
		return nil, err
	}
	wanted := false
	for _, s := range specs {
		if len(s.Disks) > 0 {
			wanted = true
		}
	}
	if wanted && !p.withheld {
		p.withheld = true
		for i := range nodes {
			nodes[i].Disks = nil // created, but vCenter has no UUID for it yet
		}
	}
	return nodes, nil
}

// step drives exactly one reconcileOne against the stored cluster. The loop advances ONE phase per
// invocation, so a test that wants the Updating body to run has to step twice from Ready: once to
// move Ready→Updating, once to run it.
func step(t *testing.T, r *Reconciler, st interface {
	GetCluster(string) (*domain.Cluster, error)
}, id string) *domain.Cluster {
	t.Helper()
	c, err := st.GetCluster(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOne(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCluster(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A disk whose identity the infrastructure hasn't reported yet must not strand the cluster: the
// update stays un-converged so the loop comes back and mounts it. Getting this wrong parks the
// cluster at Ready with a permanently pending disk - a converged Ready cluster is never reconciled
// again, so nothing would ever format it.
func TestReconcileWaitsForALateDiskIdentity(t *testing.T) {
	r, st := newTestReconciler(t)
	r.Cfg = &diskCfg{Fake: config.NewFake()}
	prov := &lateIDProv{Fake: provision.NewFake()}
	r.Prov = prov

	c := diskTestCluster("d5")
	_ = st.CreateCluster(c)
	converge(t, r, st, "d5")

	got, _ := st.GetCluster("d5")
	got.NodeDisks = []domain.NodeDisk{{
		VMName: "demo-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data",
		FSType: domain.FSExt4, Phase: domain.DiskPhasePending,
		WWN: domain.NewDiskWWN("d5", "demo-default-0", "data"),
	}}
	got.Generation++
	_ = st.UpdateCluster(got)

	// Two steps, because the loop advances one phase per invocation: Ready→Updating, then the
	// Updating body - which is the code under test.
	if mid := step(t, r, st, "d5"); mid.Phase != domain.PhaseUpdating {
		t.Fatalf("phase = %s, want Updating", mid.Phase)
	}
	mid := step(t, r, st, "d5")

	// The identity wasn't reported, so the disk can't have been mounted - and the cluster must NOT
	// claim to be converged.
	if !prov.withheld {
		t.Fatal("the provisioner never withheld an identity - the test isn't exercising the case")
	}
	if mid.NodeDisks[0].Phase == domain.DiskPhaseAttached {
		t.Fatal("a disk with no reported identity must not be marked attached - nothing formatted it")
	}
	if mid.ObservedGeneration == mid.Generation {
		t.Fatal("cluster reported converged while a disk had no identity - being Ready and converged, " +
			"it would never be reconciled again and nothing would ever mount the disk")
	}
	if !mid.NeedsWork() {
		t.Fatal("cluster must still need work while a disk is unmounted")
	}

	// Now that the identity is reported, the loop finishes the job on its own.
	converge(t, r, st, "d5")
	after, _ := st.GetCluster("d5")
	if len(after.NodeDisks) != 1 || after.NodeDisks[0].Phase != domain.DiskPhaseAttached {
		t.Fatalf("disks = %+v, want the disk mounted once its identity was reported", after.NodeDisks)
	}
	if after.NodeDisks[0].DeviceID == "" {
		t.Error("DeviceID should have been stamped on the later tick")
	}
	if after.ObservedGeneration != after.Generation {
		t.Error("cluster should be converged once the disk is mounted")
	}
}

// The reconciler builds each node's disk specs from the cluster's desired disks, keyed on the node's
// VM NAME - which is what lets a node rebuilt underneath (a rolling OS replacement mints a new node
// row) keep its disks.
func TestDiskSpecsAreKeyedOnVMName(t *testing.T) {
	c := diskTestCluster("d4")
	c.Name = "demo"
	c.NodeDisks = []domain.NodeDisk{
		{VMName: "demo-default-0", Name: "b", SizeGB: 2, WWN: "0x2"},
		{VMName: "demo-default-0", Name: "a", SizeGB: 1, WWN: "0x1"},
		{VMName: "demo-cp-0", Name: "nope", SizeGB: 9, WWN: "0x9"},
	}
	got := diskSpecs(c, "demo-default-0")
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("diskSpecs = %+v, want this node's disks in name order", got)
	}
	if got[0].SizeGB != 1 || got[0].WWN != "0x1" {
		t.Errorf("diskSpecs[0] = %+v, want the disk's own size and wwn", got[0])
	}
	if len(diskSpecs(c, "demo-default-1")) != 0 {
		t.Error("a node with no disks should get no specs")
	}
}
