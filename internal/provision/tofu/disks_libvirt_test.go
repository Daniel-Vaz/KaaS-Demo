package tofu

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/provision/tofurunner"
)

// These tests drive REAL libvirt via virsh and are gated on KAAS_TEST_LIBVIRT=1, so a normal
// `go test ./...` (no libvirt) skips them. They exist because the extra-disk attach/detach path is
// pure out-of-band virsh work that the fake provisioner can't exercise, and a wrong ordering here
// wedges `tofu destroy` (a domain left pointing at a deleted volume) - the exact failure a user hit.
//
// Run: KAAS_TEST_LIBVIRT=1 go test ./internal/provision/tofu/ -run Libvirt -v

const testURI = "qemu:///system"

func requireLibvirt(t *testing.T) {
	t.Helper()
	if os.Getenv("KAAS_TEST_LIBVIRT") != "1" {
		t.Skip("set KAAS_TEST_LIBVIRT=1 to run the libvirt integration tests")
	}
	if _, err := exec.LookPath("virsh"); err != nil {
		t.Skip("virsh not found")
	}
}

func virshT(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("virsh", append([]string{"-c", testURI}, args...)...).CombinedOutput()
	return string(out), err
}

// newTestProvisioner builds a Provisioner wired only enough for the disk helpers: they need the
// libvirt URI and a runner for Emit. No workspace/module - these tests call the virsh-level helpers
// directly, not EnsureNodes.
func newTestProvisioner(t *testing.T) *Provisioner {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return &Provisioner{
		cfg: Config{LibvirtURI: testURI, Log: log},
		run: &tofurunner.Runner{Log: log},
	}
}

// defineMinimalDomain creates a tiny non-booting domain with a virtio-scsi controller - enough to
// attach and detach SCSI disks, which is all these tests need. Returns a cleanup.
func defineMinimalDomain(t *testing.T, name string) func() {
	t.Helper()
	// A 1 GiB blank root volume, plus the domain.
	if out, err := virshT(t, "vol-create-as", "default", name+"-root.qcow2", "1G", "--format", "qcow2"); err != nil {
		t.Fatalf("create root vol: %v\n%s", err, out)
	}
	xml := `<domain type='kvm'>
  <name>` + name + `</name>
  <memory unit='MiB'>256</memory>
  <vcpu>1</vcpu>
  <os><type arch='x86_64'>hvm</type></os>
  <devices>
    <disk type='file' device='disk'><driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/` + name + `-root.qcow2'/><target dev='vda' bus='virtio'/></disk>
    <controller type='scsi' model='virtio-scsi'/>
    <graphics type='vnc' listen='127.0.0.1'/>
  </devices>
</domain>`
	f := t.TempDir() + "/dom.xml"
	if err := os.WriteFile(f, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := virshT(t, "define", f); err != nil {
		t.Fatalf("define domain: %v\n%s", err, out)
	}
	if out, err := virshT(t, "start", name); err != nil {
		t.Fatalf("start domain: %v\n%s", err, out)
	}
	return func() {
		_, _ = virshT(t, "destroy", name)
		_, _ = virshT(t, "undefine", name)
		_, _ = virshT(t, "vol-delete", name+"-root.qcow2", "--pool", "default")
		_, _ = virshT(t, "vol-delete", name+"-data.qcow2", "--pool", "default")
	}
}

func countSCSIDisks(t *testing.T, dom string) int {
	t.Helper()
	out, _ := virshT(t, "dumpxml", dom)
	return strings.Count(out, "bus='scsi'")
}

// TestLibvirtDestroyCleanupUnwedgesADomain is the regression for the reported bug: a domain left with
// a disk whose volume was already deleted (the state a removal used to leave behind) made every later
// refresh - and thus `tofu destroy` - fail forever. detachExtraDisksBeforeDestroy must clear it.
func TestLibvirtDestroyCleanupUnwedgesADomain(t *testing.T) {
	requireLibvirt(t)
	const cluster = "kaastest"
	const node = cluster + "-w-0"
	cleanup := defineMinimalDomain(t, node)
	defer cleanup()

	// Attach an extra SCSI disk the way the real code does, then delete its volume WHILE attached -
	// exactly the dangling state a buggy removal leaves behind.
	if out, err := virshT(t, "vol-create-as", "default", node+"-data.qcow2", "1G", "--format", "qcow2"); err != nil {
		t.Fatalf("create data vol: %v\n%s", err, out)
	}
	if out, err := virshT(t, "attach-disk", node, "/var/lib/libvirt/images/"+node+"-data.qcow2", "sdb",
		"--subdriver", "qcow2", "--targetbus", "scsi", "--wwn", "0x5000c50012340001", "--persistent", "--live"); err != nil {
		t.Fatalf("attach: %v\n%s", err, out)
	}
	if n := countSCSIDisks(t, node); n != 1 {
		t.Fatalf("expected 1 scsi disk attached, got %d", n)
	}
	if out, err := virshT(t, "vol-delete", node+"-data.qcow2", "--pool", "default"); err != nil {
		t.Fatalf("delete vol: %v\n%s", err, out)
	}

	// The recovery under test.
	p := newTestProvisioner(t)
	p.detachExtraDisksBeforeDestroy(context.Background(), cluster)

	if n := countSCSIDisks(t, node); n != 0 {
		t.Fatalf("scsi disks after cleanup = %d, want 0 - the dangling disk must be detached so tofu can destroy the domain", n)
	}
}

// TestLibvirtAttachDetachRoundTrip proves the two building blocks the ordering relies on: an attach
// via the real helper appears in the domain, and a detach removes it - live and persistent, with the
// wwn as the stable handle.
func TestLibvirtAttachDetachRoundTrip(t *testing.T) {
	requireLibvirt(t)
	const cluster = "kaasrt"
	const node = cluster + "-w-0"
	cleanup := defineMinimalDomain(t, node)
	defer cleanup()

	if out, err := virshT(t, "vol-create-as", "default", node+"-data.qcow2", "1G", "--format", "qcow2"); err != nil {
		t.Fatalf("create vol: %v\n%s", err, out)
	}
	p := newTestProvisioner(t)
	dom := domainNameFor(cluster, "w-0")

	// Attach through the real helper, then confirm domainDisks observes it by wwn.
	if err := p.virsh(context.Background(), cluster, true, "attach-disk", dom,
		"/var/lib/libvirt/images/"+node+"-data.qcow2", "sdb",
		"--subdriver", "qcow2", "--targetbus", "scsi", "--wwn", "0x5000c50012340002"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	set, err := p.domainDisks(context.Background(), dom)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Running {
		t.Fatal("domain should be running")
	}
	if set.ByWWN["5000c50012340002"] != "sdb" {
		t.Fatalf("domainDisks = %v, want the wwn mapped to sdb", set.ByWWN)
	}
	// The root disk carries no wwn, so it is absent from ByWWN but must still be counted as a target
	// in use - that wider set is what freeTarget picks against.
	if !set.UsedTargets["vda"] || !set.UsedTargets["sdb"] {
		t.Fatalf("UsedTargets = %v, want both the root disk (vda) and the extra disk (sdb)", set.UsedTargets)
	}

	// Detach through the real helper; the disk must be gone.
	if err := p.virsh(context.Background(), cluster, true, "detach-disk", dom, "sdb"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	set, err = p.domainDisks(context.Background(), dom)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.ByWWN) != 0 {
		t.Fatalf("domainDisks after detach = %v, want empty", set.ByWWN)
	}
}
