package domain

import "testing"

// A cluster with one worker pool, for the disk validation tests.
func diskCluster() *Cluster {
	return &Cluster{
		ID:            "cid",
		Name:          "c",
		Size:          "small",
		ControlPlanes: 1,
		NodePools:     []NodePool{{Name: "default", Size: "small", DesiredWorkers: 2}},
	}
}

// A disk may only sit on a worker the cluster actually desires. A control plane is off limits (etcd
// lives there - its storage is the platform's business), and a VM name DesiredNodes never mints is
// desired state nothing would ever converge.
func TestValidateNodeDisksNodeMustBeADesiredWorker(t *testing.T) {
	c := diskCluster()
	ok := NodeDisk{VMName: "c-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data", FSType: FSExt4}
	if err := ValidateNodeDisks(c, []NodeDisk{ok}); err != nil {
		t.Fatalf("disk on a desired worker should be valid: %v", err)
	}
	cp := ok
	cp.VMName = "c-cp-0"
	if err := ValidateNodeDisks(c, []NodeDisk{cp}); err == nil {
		t.Fatal("a disk on a control plane should be rejected")
	}
	ghost := ok
	ghost.VMName = "c-default-9" // beyond the pool's desired workers
	if err := ValidateNodeDisks(c, []NodeDisk{ghost}); err == nil {
		t.Fatal("a disk on a node the cluster doesn't desire should be rejected")
	}
}

// Mount paths that would shadow the running system are refused. This is the one validation standing
// between a plausible-looking request and a destroyed node.
func TestValidateMountPathRejectsSystemDirs(t *testing.T) {
	for _, p := range []string{"/", "/etc", "/var", "/var/lib/kubelet", "/etc/kubernetes", "/usr"} {
		if err := ValidateMountPath(p); err == nil {
			t.Errorf("mount path %q should be rejected - a fresh filesystem there hides the running system", p)
		}
	}
	// A path BELOW a protected one is the normal case and must stay allowed.
	for _, p := range []string{"/var/lib/data", "/mnt/disk1", "/srv/data"} {
		if err := ValidateMountPath(p); err != nil {
			t.Errorf("mount path %q should be allowed: %v", p, err)
		}
	}
	for _, p := range []string{"", "relative/path", "/trailing/", "/dots/../x"} {
		if err := ValidateMountPath(p); err == nil {
			t.Errorf("mount path %q should be rejected as malformed", p)
		}
	}
}

// Two disks on ONE node cannot share a mount path (the second would shadow the first, silently
// costing the node a disk) - but the same path across DIFFERENT nodes is the normal shape.
func TestValidateNodeDisksMountPathCollision(t *testing.T) {
	c := diskCluster()
	same := []NodeDisk{
		{VMName: "c-default-0", Name: "a", SizeGB: 10, MountPath: "/var/lib/data", FSType: FSExt4},
		{VMName: "c-default-0", Name: "b", SizeGB: 10, MountPath: "/var/lib/data", FSType: FSExt4},
	}
	if err := ValidateNodeDisks(c, same); err == nil {
		t.Fatal("two disks on one node sharing a mount path should be rejected")
	}
	across := []NodeDisk{
		{VMName: "c-default-0", Name: "a", SizeGB: 10, MountPath: "/var/lib/data", FSType: FSExt4},
		{VMName: "c-default-1", Name: "a", SizeGB: 10, MountPath: "/var/lib/data", FSType: FSExt4},
	}
	if err := ValidateNodeDisks(c, across); err != nil {
		t.Fatalf("the same mount path on different nodes is normal, not a conflict: %v", err)
	}
}

func TestValidateNodeDisksNameAndSize(t *testing.T) {
	c := diskCluster()
	base := NodeDisk{VMName: "c-default-0", Name: "data", SizeGB: 10, MountPath: "/var/lib/data", FSType: FSExt4}

	dup := []NodeDisk{base, base}
	if err := ValidateNodeDisks(c, dup); err == nil {
		t.Fatal("two disks with the same name on one node should be rejected")
	}
	for _, bad := range []string{"", "Data", "data_1", "-data", "data-"} {
		d := base
		d.Name = bad
		if err := ValidateNodeDisks(c, []NodeDisk{d}); err == nil {
			t.Errorf("disk name %q should be rejected (it becomes an LVM volume-group name)", bad)
		}
	}
	for _, sz := range []int{0, -1, MaxDiskGB + 1} {
		d := base
		d.SizeGB = sz
		if err := ValidateNodeDisks(c, []NodeDisk{d}); err == nil {
			t.Errorf("disk size %d should be rejected", sz)
		}
	}
	d := base
	d.FSType = "btrfs"
	if err := ValidateNodeDisks(c, []NodeDisk{d}); err == nil {
		t.Fatal("an unsupported filesystem should be rejected")
	}
}

func TestValidateNodeDisksPerNodeCap(t *testing.T) {
	c := diskCluster()
	var disks []NodeDisk
	for i := 0; i <= MaxDisksPerNode; i++ {
		disks = append(disks, NodeDisk{
			VMName: "c-default-0", Name: string(rune('a' + i)), SizeGB: 1,
			MountPath: "/mnt/" + string(rune('a'+i)), FSType: FSExt4,
		})
	}
	if err := ValidateNodeDisks(c, disks); err == nil {
		t.Fatalf("more than %d disks on one node should be rejected", MaxDisksPerNode)
	}
}

// The WWN is the guest's handle on the device, so it must be stable (recomputed identically forever)
// and distinct per disk - a collision would mean formatting the wrong device.
func TestNewDiskWWN(t *testing.T) {
	a := NewDiskWWN("cid", "c-default-0", "data")
	if a != NewDiskWWN("cid", "c-default-0", "data") {
		t.Fatal("NewDiskWWN must be deterministic - it is recomputed, never stored, and pins a live device")
	}
	// libvirt requires exactly 16 hex digits after the 0x and rejects anything else.
	if len(a) != 18 || a[:2] != "0x" {
		t.Fatalf("wwn = %q, want 0x + 16 hex digits", a)
	}
	for _, other := range []string{
		NewDiskWWN("cid", "c-default-0", "logs"), // same node, different disk
		NewDiskWWN("cid", "c-default-1", "data"), // different node, same disk name
		NewDiskWWN("other", "c-default-0", "data"),
	} {
		if a == other {
			t.Fatalf("wwn collision: %q - two disks would resolve to one device", a)
		}
	}
}

func TestNodeDiskVolumeGroupIsPerDisk(t *testing.T) {
	// One VG per disk is what keeps each disk independently removable: no LV can span another
	// disk's extents, so tearing one down can't strand another.
	a := NodeDisk{Name: "data"}
	b := NodeDisk{Name: "logs"}
	if a.VolumeGroup() == b.VolumeGroup() {
		t.Fatal("two disks on a node must not share a volume group")
	}
}

func TestDisksFor(t *testing.T) {
	c := diskCluster()
	c.NodeDisks = []NodeDisk{
		{VMName: "c-default-1", Name: "z"},
		{VMName: "c-default-0", Name: "b"},
		{VMName: "c-default-0", Name: "a"},
	}
	got := DisksFor(c, "c-default-0")
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("DisksFor = %+v, want this node's disks in name order", got)
	}
}

// The platform's own storage disk is mounted at Longhorn's data path, which IS the registration
// mechanism - and precisely because Longhorn discovers that path itself, the platform must not also
// register it (Longhorn refuses two disks on one node sharing a path).
func TestStoragePoolMembership(t *testing.T) {
	platform := NodeDisk{Name: PlatformStorageDiskName, MountPath: LonghornMountPath(PlatformStorageDiskName),
		Phase: DiskPhaseAttached}
	extra := NodeDisk{Name: "extra", MountPath: LonghornMountPath("extra"), Phase: DiskPhaseAttached}
	plain := NodeDisk{Name: "plain", MountPath: "/var/lib/plain", Phase: DiskPhaseAttached}

	if platform.MountPath != LonghornDataPath {
		t.Fatalf("the platform disk must take Longhorn's own data path, got %q", platform.MountPath)
	}
	for _, tc := range []struct {
		d          NodeDisk
		pool, regs bool
	}{
		{platform, true, false},
		{extra, true, true},
		{plain, false, false},
	} {
		if got := tc.d.FeedsStoragePool(); got != tc.pool {
			t.Errorf("%s: FeedsStoragePool = %v, want %v", tc.d.Name, got, tc.pool)
		}
		if got := tc.d.NeedsLonghornRegistration(); got != tc.regs {
			t.Errorf("%s: NeedsLonghornRegistration = %v, want %v", tc.d.Name, got, tc.regs)
		}
	}
	// A pending disk has no filesystem yet - registering its path would point Longhorn at the root
	// disk.
	pending := extra
	pending.Phase = DiskPhasePending
	if pending.NeedsLonghornRegistration() {
		t.Error("a pending disk must not be registered")
	}
}

// The fingerprint is what makes the wiring re-run when the disk set changes and skip when it hasn't.
func TestStorageFingerprintTracksRegisteredDisks(t *testing.T) {
	c := &Cluster{NodeDisks: []NodeDisk{
		{VMName: "n0", Name: PlatformStorageDiskName, MountPath: LonghornDataPath, Phase: DiskPhaseAttached},
	}}
	if got := StorageFingerprint(c); got != "" {
		t.Fatalf("fingerprint = %q, want empty - the platform disk registers itself", got)
	}
	c.NodeDisks = append(c.NodeDisks, NodeDisk{
		VMName: "n0", Name: "extra", MountPath: LonghornMountPath("extra"), Phase: DiskPhaseAttached})
	first := StorageFingerprint(c)
	if first == "" {
		t.Fatal("an extra pool disk must change the fingerprint")
	}
	// Stable across an unrelated edit...
	c.Generation++
	if StorageFingerprint(c) != first {
		t.Fatal("the fingerprint must not move on an unrelated change")
	}
	// ...and moves when the disk goes.
	c.NodeDisks = c.NodeDisks[:1]
	if StorageFingerprint(c) == first {
		t.Fatal("removing the disk must change the fingerprint")
	}
}
