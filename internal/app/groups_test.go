package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

func TestGroupCRUDAdminOnly(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")

	if _, err := a.CreateGroup(alice, "team-a"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin CreateGroup = %v, want ErrForbidden", err)
	}
	g, err := a.CreateGroup(ad, "team-a")
	if err != nil {
		t.Fatalf("admin CreateGroup: %v", err)
	}
	if _, err := a.CreateGroup(ad, "team-a"); err == nil {
		t.Fatal("duplicate group name should be rejected")
	}

	if _, err := a.RenameGroup(ad, g.ID, "team-alpha"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	groups, err := a.ListGroups(ad)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != "team-alpha" {
		t.Fatalf("ListGroups = %+v, want one renamed group", groups)
	}
	if _, err := a.ListGroups(alice); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin ListGroups = %v, want ErrForbidden", err)
	}
}

func TestDeleteGroupUngroupsMembersOnly(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	g, _ := a.CreateGroup(ad, "team-a")
	alice = grantQuota(t, a, alice.ID, 4, 8192)
	if _, err := a.UpdateUser(ad, alice.ID, UpdateUserRequest{Memberships: memberships(g.ID, domain.GroupRoleRead)}); err != nil {
		t.Fatalf("assign group: %v", err)
	}
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	if err := a.DeleteGroup(ad, g.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	got, err := a.Store.GetUser(alice.ID)
	if err != nil {
		t.Fatalf("reload alice: %v", err)
	}
	if len(got.Memberships) != 0 {
		t.Fatalf("alice should be ungrouped after group delete, got memberships=%+v", got.Memberships)
	}
	// The cluster is untouched by the group deletion.
	cluster, err := a.Store.GetCluster(c.ID)
	if err != nil {
		t.Fatalf("reload cluster: %v", err)
	}
	if cluster.Phase.Terminal() || cluster.Phase == "Deleting" {
		t.Fatalf("cluster should be unaffected by group deletion, got phase=%s", cluster.Phase)
	}
}

func TestUpdateUserUnknownGroupRejected(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	if _, err := a.UpdateUser(ad, alice.ID, UpdateUserRequest{Memberships: memberships("does-not-exist", domain.GroupRoleRead)}); err == nil {
		t.Fatal("assigning an unknown group should be rejected")
	}
}

// memberships builds a single-entry membership set for the UpdateUser request (the common case in
// these tests). Returns a pointer, since the request field is a pointer-to-slice (nil = unchanged).
func memberships(groupID string, role domain.GroupRole) *[]domain.GroupMembership {
	return &[]domain.GroupMembership{{GroupID: groupID, Role: role}}
}

// assignGroup puts a user in a single group with a role (admin-only edit, replacing any prior
// memberships) and returns them freshly reloaded - callers must use the reloaded user as the actor,
// since authorization reads the actor's current memberships (in production the actor always comes
// fresh from the session).
func assignGroup(t *testing.T, a *App, userID, groupID string, role domain.GroupRole) *domain.User {
	t.Helper()
	ad := admin(t, a)
	if _, err := a.UpdateUser(ad, userID, UpdateUserRequest{Memberships: memberships(groupID, role)}); err != nil {
		t.Fatalf("assign group/role: %v", err)
	}
	u, err := a.Store.GetUser(userID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	return u
}

// TestGroupWriteRoleFullAccess: a group-mate with the Write role gets full access to the other
// members' clusters (view, scale, delete) - the same as the owner. A third, ungrouped user still
// gets a 404, matching plain ownership isolation.
func TestGroupWriteRoleFullAccess(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	bob, _ := a.Register("bob", "password")
	carol, _ := a.Register("carol", "password")
	alice = grantQuota(t, a, alice.ID, 8, 16384) // enough to scale her small cluster to two nodes
	_ = grantQuota(t, a, bob.ID, 4, 8192)
	carol = grantQuota(t, a, carol.ID, 4, 8192)

	g, err := a.CreateGroup(ad, "team-a")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	alice = assignGroup(t, a, alice.ID, g.ID, domain.GroupRoleWrite)
	bob = assignGroup(t, a, bob.ID, g.ID, domain.GroupRoleWrite)

	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("alice create: %v", err)
	}

	// Bob (write-role group-mate) gets full access: view, scale, and eventually delete.
	if _, err := a.GetCluster(bob, c.ID); err != nil {
		t.Fatalf("group-mate GetCluster: %v", err)
	}
	if _, err := a.UpdateCluster(bob, c.ID, UpdateRequest{NodePools: ptr(pools(1))}); err != nil {
		t.Fatalf("group-mate UpdateCluster (scale): %v", err)
	}

	// Carol (not in the group) still gets a 404, same as before groups existed.
	if _, err := a.GetCluster(carol, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-group-mate GetCluster = %v, want ErrNotFound", err)
	}
	if err := a.DeleteCluster(carol, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-group-mate DeleteCluster = %v, want ErrNotFound", err)
	}

	// ListClusters for a group-mate includes clusters owned by other members.
	bobList, err := a.ListClusters(bob)
	if err != nil {
		t.Fatalf("bob ListClusters: %v", err)
	}
	if len(bobList) != 1 {
		t.Fatalf("bob sees %d clusters via group membership, want 1", len(bobList))
	}
	carolList, err := a.ListClusters(carol)
	if err != nil {
		t.Fatalf("carol ListClusters: %v", err)
	}
	if len(carolList) != 0 {
		t.Fatalf("carol (no group) sees %d clusters, want 0", len(carolList))
	}

	// Bob (write-role group-mate) can delete alice's cluster too - full access.
	if err := a.DeleteCluster(bob, c.ID); err != nil {
		t.Fatalf("group-mate DeleteCluster: %v", err)
	}
}

// TestGroupReadRoleViewOnly is the RBAC boundary: a Read-role group-mate can SEE another member's
// cluster (it's in their list, GetCluster succeeds) but every mutating action is refused with
// ErrForbidden - scale, delete, upgrade, and the admin kubeconfig. Their own clusters are
// unaffected: Read only ever gates access to OTHER members' clusters.
func TestGroupReadRoleViewOnly(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	bob, _ := a.Register("bob", "password")
	alice = grantQuota(t, a, alice.ID, 8, 16384) // enough to scale her small cluster to two nodes
	bob = grantQuota(t, a, bob.ID, 8, 16384)     // enough to scale his own small cluster to two nodes

	g, err := a.CreateGroup(ad, "team-a")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	alice = assignGroup(t, a, alice.ID, g.ID, domain.GroupRoleWrite)
	bob = assignGroup(t, a, bob.ID, g.ID, domain.GroupRoleRead) // read-only member

	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("alice create: %v", err)
	}

	// Bob can see it (view) - it appears in his list and GetCluster succeeds.
	if _, err := a.GetCluster(bob, c.ID); err != nil {
		t.Fatalf("read-role GetCluster should succeed: %v", err)
	}
	bobList, _ := a.ListClusters(bob)
	if len(bobList) != 1 {
		t.Fatalf("read-role member sees %d clusters, want 1", len(bobList))
	}

	// But every write is ErrForbidden (visible, so not a 404 - an honest 403).
	if _, err := a.UpdateCluster(bob, c.ID, UpdateRequest{NodePools: ptr(pools(1))}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-role UpdateCluster = %v, want ErrForbidden", err)
	}
	if err := a.DeleteCluster(bob, c.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-role DeleteCluster = %v, want ErrForbidden", err)
	}
	if a.CanManageCluster(bob, c) {
		t.Fatal("read-role CanManageCluster = true, want false (may not mutate)")
	}

	// UserKubeconfig (the shell/Workloads credential) mints each role a per-user cert carrying their
	// OWN identity. Seed the admin config (the minting authority) and a fake kube backend.
	a.Kube = kube.NewFake()
	seedSecret(t, a, c.ID, domain.SecretKubeconfig, "admin-kubeconfig")

	// Alice (write role): a writer cert (not read-only), her own CN and the writers group.
	if kc, ro, err := a.UserKubeconfig(context.Background(), alice, c.ID); err != nil || ro || !strings.Contains(string(kc), "CN=alice, O="+domain.KubeGroupWriters) {
		t.Fatalf("write-role UserKubeconfig = (%q, ro=%v, %v), want writer identity, ro=false", kc, ro, err)
	}
	// Bob (read role): a reader cert marked read-only, his own CN and the readers group.
	if kc, ro, err := a.UserKubeconfig(context.Background(), bob, c.ID); err != nil || !ro || !strings.Contains(string(kc), "CN=bob, O="+domain.KubeGroupReaders) {
		t.Fatalf("read-role UserKubeconfig = (%q, ro=%v, %v), want reader identity, ro=true", kc, ro, err)
	}

	// Bob keeps full control of his OWN cluster despite being read-role in the group.
	own, err := a.CreateCluster(bob, CreateRequest{Name: "bobs", Size: "small"})
	if err != nil {
		t.Fatalf("bob create own: %v", err)
	}
	if _, err := a.UpdateCluster(bob, own.ID, UpdateRequest{NodePools: ptr(pools(1))}); err != nil {
		t.Fatalf("read-role member managing their OWN cluster: %v", err)
	}

	// Promoting alice's default role away from the RBAC boundary shouldn't matter here, but confirm
	// an admin can flip bob to write and the block lifts.
	bob = assignGroup(t, a, bob.ID, g.ID, domain.GroupRoleWrite)
	if _, err := a.UpdateCluster(bob, c.ID, UpdateRequest{NodePools: ptr(pools(1))}); err != nil {
		t.Fatalf("after promotion to write, UpdateCluster: %v", err)
	}
}

// TestRegisterStartsUngrouped: a self-registered account starts in no groups (least privilege - it
// can only see its own clusters until an admin adds it to a group).
func TestRegisterStartsUngrouped(t *testing.T) {
	a := newTenancyApp(t)
	alice, err := a.Register("alice", "password")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(alice.Memberships) != 0 {
		t.Fatalf("new user should start ungrouped, got memberships=%+v", alice.Memberships)
	}
}

// TestUpdateUserInvalidRoleRejected: only "read"/"write" are accepted role values.
func TestUpdateUserInvalidRoleRejected(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	g, _ := a.CreateGroup(ad, "team-a")
	if _, err := a.UpdateUser(ad, alice.ID, UpdateUserRequest{Memberships: memberships(g.ID, domain.GroupRole("superuser"))}); err == nil {
		t.Fatal("an unknown role should be rejected")
	}
}

// TestMultiGroupPerGroupRoles: a user in two groups holds an independent role in each - Read in one
// (view-only over that group's clusters) and Write in the other (full management) - and the higher
// role never leaks across to the other group.
func TestMultiGroupPerGroupRoles(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password") // owner in group-r
	bob, _ := a.Register("bob", "password")     // owner in group-w
	dan, _ := a.Register("dan", "password")     // read in group-r, write in group-w
	alice = grantQuota(t, a, alice.ID, 4, 8192)
	bob = grantQuota(t, a, bob.ID, 8, 16384) // enough to scale his cluster to two nodes
	_ = grantQuota(t, a, dan.ID, 4, 8192)

	gr, _ := a.CreateGroup(ad, "group-r")
	gw, _ := a.CreateGroup(ad, "group-w")
	alice = assignGroup(t, a, alice.ID, gr.ID, domain.GroupRoleWrite)
	bob = assignGroup(t, a, bob.ID, gw.ID, domain.GroupRoleWrite)
	// Dan joins both groups, Read in group-r and Write in group-w.
	if _, err := a.UpdateUser(ad, dan.ID, UpdateUserRequest{Memberships: &[]domain.GroupMembership{
		{GroupID: gr.ID, Role: domain.GroupRoleRead},
		{GroupID: gw.ID, Role: domain.GroupRoleWrite},
	}}); err != nil {
		t.Fatalf("assign dan two memberships: %v", err)
	}
	dan, _ = a.Store.GetUser(dan.ID)

	aliceCluster, err := a.CreateCluster(alice, CreateRequest{Name: "adev", Size: "small"})
	if err != nil {
		t.Fatalf("alice create: %v", err)
	}
	bobCluster, err := a.CreateCluster(bob, CreateRequest{Name: "bdev", Size: "small"})
	if err != nil {
		t.Fatalf("bob create: %v", err)
	}

	// Dan sees both clusters (shares a group with each owner).
	danList, _ := a.ListClusters(dan)
	if len(danList) != 2 {
		t.Fatalf("dan sees %d clusters, want 2", len(danList))
	}

	// Read in group-r: dan may view alice's cluster but not mutate it.
	if _, err := a.GetCluster(dan, aliceCluster.ID); err != nil {
		t.Fatalf("dan GetCluster(alice): %v", err)
	}
	if _, err := a.UpdateCluster(dan, aliceCluster.ID, UpdateRequest{NodePools: ptr(pools(1))}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("dan (read in group-r) UpdateCluster(alice) = %v, want ErrForbidden", err)
	}

	// Write in group-w: dan may fully manage bob's cluster.
	if _, err := a.UpdateCluster(dan, bobCluster.ID, UpdateRequest{NodePools: ptr(pools(1))}); err != nil {
		t.Fatalf("dan (write in group-w) UpdateCluster(bob): %v", err)
	}
}

// TestUpdateUserDuplicateGroupRejected: a membership set can't name the same group twice.
func TestUpdateUserDuplicateGroupRejected(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	g, _ := a.CreateGroup(ad, "team-a")
	req := UpdateUserRequest{Memberships: &[]domain.GroupMembership{
		{GroupID: g.ID, Role: domain.GroupRoleRead},
		{GroupID: g.ID, Role: domain.GroupRoleWrite},
	}}
	if _, err := a.UpdateUser(ad, alice.ID, req); err == nil {
		t.Fatal("duplicate group membership should be rejected")
	}
}

func TestOwnerUsernames(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	alice = grantQuota(t, a, alice.ID, 4, 8192)
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	names, err := a.OwnerUsernames([]*domain.Cluster{c})
	if err != nil {
		t.Fatalf("OwnerUsernames: %v", err)
	}
	if names[c.OwnerID] != "alice" {
		t.Fatalf("owner username = %q, want alice", names[c.OwnerID])
	}
}

// TestDownloadKubeconfigPerUserIdentity: the tenant-facing download mints a per-USER credential -
// the actor's own login as CN and their RESOLVED access as the cert's group - uniformly for owner,
// write-role and read-role members, and refuses a stranger. This is the identity the cluster's RBAC
// binds (kaas:writers → cluster-admin, kaas:readers → view), so cluster access mirrors the portal.
func TestDownloadKubeconfigPerUserIdentity(t *testing.T) {
	a := newTenancyApp(t)
	a.Kube = kube.NewFake()
	a.userKubeconfigTTL = 30 * 24 * time.Hour
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	bob, _ := a.Register("bob", "password")
	carol, _ := a.Register("carol", "password") // shares no group with alice
	alice = grantQuota(t, a, alice.ID, 8, 16384)

	g, err := a.CreateGroup(ad, "team-a")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	alice = assignGroup(t, a, alice.ID, g.ID, domain.GroupRoleWrite)
	bob = assignGroup(t, a, bob.ID, g.ID, domain.GroupRoleRead)

	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("alice create: %v", err)
	}
	// The admin config is the minting authority and the endpoint/CA source; the reconciler stores it
	// once the cluster is Ready.
	seedSecret(t, a, c.ID, domain.SecretKubeconfig, "apiVersion: v1\nkind: Config\nclusters:\n- name: dev\n  cluster:\n    server: https://10.0.0.1:6443\n")

	// Owner (write role): their own CN, the writers group, not read-only, with an expiry.
	kcA, roA, expA, err := a.DownloadKubeconfig(context.Background(), alice, c.ID)
	if err != nil || roA {
		t.Fatalf("alice download = (ro=%v, %v), want ro=false", roA, err)
	}
	if !strings.Contains(string(kcA), "CN=alice, O="+domain.KubeGroupWriters) {
		t.Fatalf("alice kubeconfig missing writer identity:\n%s", kcA)
	}
	if expA.IsZero() {
		t.Fatal("alice download reported no expiry")
	}

	// Read-role group-mate: their own CN, the readers group, marked read-only.
	kcB, roB, _, err := a.DownloadKubeconfig(context.Background(), bob, c.ID)
	if err != nil || !roB {
		t.Fatalf("bob download = (ro=%v, %v), want ro=true", roB, err)
	}
	if !strings.Contains(string(kcB), "CN=bob, O="+domain.KubeGroupReaders) {
		t.Fatalf("bob kubeconfig missing reader identity:\n%s", kcB)
	}

	// A user who shares no group can't see the cluster - a 404, and nothing is minted.
	if _, _, _, err := a.DownloadKubeconfig(context.Background(), carol, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("carol download err = %v, want store.ErrNotFound", err)
	}
}

// countingKube counts MintUserKubeconfig calls, to prove the per-user cache avoids re-minting.
type countingKube struct {
	*kube.Fake
	mints int
}

func (c *countingKube) MintUserKubeconfig(ctx context.Context, cl *domain.Cluster, admin []byte, user string, role domain.GroupRole, ttl time.Duration) ([]byte, time.Time, error) {
	c.mints++
	return c.Fake.MintUserKubeconfig(ctx, cl, admin, user, role, ttl)
}

// TestUserKubeconfigCached: the interactive seams mint a user's cert once and reuse it - a page of
// Workloads calls must not run the CSR dance every time.
func TestUserKubeconfigCached(t *testing.T) {
	a := newTenancyApp(t)
	ck := &countingKube{Fake: kube.NewFake()}
	a.Kube = ck
	a.userKubeconfigTTL = 30 * 24 * time.Hour
	alice, _ := a.Register("alice", "password")
	alice = grantQuota(t, a, alice.ID, 8, 16384)
	c, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSecret(t, a, c.ID, domain.SecretKubeconfig, "admin-kubeconfig")

	for i := 0; i < 3; i++ {
		if _, _, err := a.UserKubeconfig(context.Background(), alice, c.ID); err != nil {
			t.Fatalf("UserKubeconfig #%d: %v", i, err)
		}
	}
	if ck.mints != 1 {
		t.Fatalf("minted %d times across 3 calls, want 1 (cached)", ck.mints)
	}
}
