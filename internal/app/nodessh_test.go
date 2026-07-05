package app

import (
	"errors"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// withNode gives a created cluster one observed worker node with an IP, standing in for what the
// provisioner would record - NodeSSHTarget resolves the node and its IP from this.
func withNode(t *testing.T, a *App, id, vmName, ip string) {
	t.Helper()
	c, err := a.Store.GetCluster(id)
	if err != nil {
		t.Fatalf("get cluster %s: %v", id, err)
	}
	c.Nodes = append(c.Nodes, domain.Node{ID: vmName, Role: domain.RoleWorker, VMName: vmName, IP: ip, Phase: "running"})
	if err := a.Store.UpdateCluster(c); err != nil {
		t.Fatalf("update cluster %s: %v", id, err)
	}
}

// TestNodeSSHTargetAccess: node SSH gates on WRITE access. The owner and a write-role group-mate
// resolve the node; a read-role group-mate is Forbidden (403); a complete stranger gets NotFound
// (404, indistinguishable from a cluster that doesn't exist - no cross-tenant probing). This mirrors
// authorizeClusterWrite, which the handler relies on.
func TestNodeSSHTargetAccess(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)

	alice, _ := a.Register("alice", "password")
	alice = grantQuota(t, a, alice.ID, 8, 16384)
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	withNode(t, a, c.ID, "dev-default-0", "10.1.2.3")

	// A group both a reader and a writer belong to alongside alice.
	g, _ := a.CreateGroup(ad, "team")
	alice = assignGroup(t, a, alice.ID, g.ID, domain.GroupRoleWrite)
	reader, _ := a.Register("reader", "password")
	reader = assignGroup(t, a, reader.ID, g.ID, domain.GroupRoleRead)
	writer, _ := a.Register("writer", "password")
	writer = assignGroup(t, a, writer.ID, g.ID, domain.GroupRoleWrite)
	stranger, _ := a.Register("stranger", "password")

	// Owner resolves the node, with its IP intact.
	gotC, gotN, err := a.NodeSSHTarget(alice, c.ID, "dev-default-0")
	if err != nil {
		t.Fatalf("owner NodeSSHTarget: %v", err)
	}
	if gotC.ID != c.ID || gotN.VMName != "dev-default-0" || gotN.IP != "10.1.2.3" {
		t.Fatalf("owner got cluster=%s node=%s ip=%s", gotC.ID, gotN.VMName, gotN.IP)
	}

	// Write-role group-mate: same as the owner.
	if _, _, err := a.NodeSSHTarget(writer, c.ID, "dev-default-0"); err != nil {
		t.Fatalf("write-role group-mate NodeSSHTarget: %v", err)
	}

	// Admin: full access.
	if _, _, err := a.NodeSSHTarget(ad, c.ID, "dev-default-0"); err != nil {
		t.Fatalf("admin NodeSSHTarget: %v", err)
	}

	// Read-role group-mate: Forbidden (they can SEE the cluster, so 403 is the honest answer).
	if _, _, err := a.NodeSSHTarget(reader, c.ID, "dev-default-0"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-role group-mate = %v, want ErrForbidden", err)
	}

	// Stranger: NotFound, not Forbidden - no probing for others' clusters.
	if _, _, err := a.NodeSSHTarget(stranger, c.ID, "dev-default-0"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stranger = %v, want ErrNotFound", err)
	}
}

// TestNodeSSHTargetUnknownNode: a VM name the cluster doesn't have is NotFound (the browser named a
// VM; a wrong name is indistinguishable from one that never existed), even for a full-access actor.
func TestNodeSSHTargetUnknownNode(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	alice = grantQuota(t, a, alice.ID, 8, 16384)
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	withNode(t, a, c.ID, "dev-default-0", "10.1.2.3")

	if _, _, err := a.NodeSSHTarget(alice, c.ID, "dev-default-9"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown node = %v, want ErrNotFound", err)
	}
}

// TestNodeSSHTargetNoIP: a node that exists but has no IP yet resolves successfully (the handler,
// not NodeSSHTarget, reports the not-yet-provisioned state in-terminal) - the target is found, the
// IP is simply empty.
func TestNodeSSHTargetNoIP(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	alice = grantQuota(t, a, alice.ID, 8, 16384)
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	withNode(t, a, c.ID, "dev-default-0", "") // no IP yet

	_, n, err := a.NodeSSHTarget(alice, c.ID, "dev-default-0")
	if err != nil {
		t.Fatalf("NodeSSHTarget with empty IP: %v", err)
	}
	if n.IP != "" {
		t.Fatalf("expected empty IP, got %q", n.IP)
	}
	// AuditNodeSSH with the test App's nil broker must be a harmless no-op.
	a.AuditNodeSSH(c, alice, n, "opened")
}
