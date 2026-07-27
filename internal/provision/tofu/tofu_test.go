package tofu

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/provision/tofurunner"
)

// TestListManagedIgnoresNonWorkspaceDirs is a regression test for the orphan-GC bug where the shell
// and kube seams' per-cluster scratch dirs (which share WorkDir with the tofu workspaces) were
// reported as managed clusters, so the GC destroyed them - deleting the kubeconfig out from under
// live terminal sessions. ListManaged must count only real workspaces (dirs holding `.tf` files).
func TestListManagedIgnoresNonWorkspaceDirs(t *testing.T) {
	work := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := &Provisioner{cfg: Config{WorkDir: work}, run: &tofurunner.Runner{WorkDir: work, Log: log}}

	// A real workspace: a cluster-id dir containing the module's copied .tf files.
	realWS := filepath.Join(work, "abc123deadbeef01")
	if err := os.MkdirAll(realWS, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realWS, "main.tf"), []byte("# module\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bookkeeping dirs that share WorkDir but are NOT workspaces (no .tf files). The shell one is
	// even nested, mirroring WorkDir/shell/<cluster-id>/<session-id>.
	shellSession := filepath.Join(work, "shell", "abc123deadbeef01", "sess01")
	if err := os.MkdirAll(shellSession, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shellSession, "kubeconfig"), []byte("kube\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "kube", "abc123deadbeef01"), 0o755); err != nil {
		t.Fatal(err)
	}

	managed, err := p.ListManaged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(managed, []string{"abc123deadbeef01"}) {
		t.Fatalf("ListManaged = %v, want only the real workspace [abc123deadbeef01]; shell/kube must be excluded", managed)
	}
}

// TestFreeTargetAvoidsTheCloudInitCDROM pins the reason freeTarget is given EVERY target device
// rather than only the wwn-bearing ones: the module declares the cloud-init CD-ROM at sda, and
// libvirt refuses an attach onto a device that already exists ("target sda already exists"). It also
// pins that the first hot-attached disk lands on sdb - the same device the module declares it at
// when a node is created or rebuilt, so the two paths converge on the same layout.
func TestFreeTargetAvoidsTheCloudInitCDROM(t *testing.T) {
	// A freshly created node: virtio root at vda, cloud-init CD-ROM at sda, no extra disks yet.
	used := map[string]bool{"vda": true, "sda": true}
	if got := freeTarget(used); got != "sdb" {
		t.Fatalf("freeTarget = %q, want sdb - sda is the cloud-init CD-ROM", got)
	}
	used["sdb"] = true
	if got := freeTarget(used); got != "sdc" {
		t.Fatalf("freeTarget = %q, want sdc", got)
	}
}
