package vault

import (
	"context"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// mkUser is a terse constructor for a user with memberships.
func mkUser(id, name string, admin bool, memberships ...domain.GroupMembership) *domain.User {
	return &domain.User{ID: id, Username: name, IsAdmin: admin, Memberships: memberships}
}

// findGroup / findEntity locate a desired object by name for assertions.
func findGroup(s DesiredState, name string) *DesiredGroup {
	for i := range s.Groups {
		if s.Groups[i].Name == name {
			return &s.Groups[i]
		}
	}
	return nil
}

func findEntity(s DesiredState, name string) *DesiredEntity {
	for i := range s.Entities {
		if s.Entities[i].Name == name {
			return &s.Entities[i]
		}
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// TestDesiredAccessMirrorsAccessTo is the load-bearing test: the Vault objects DesiredAccess produces
// must mirror app.accessTo exactly. Scenario - alice owns c1 and is WRITE in group eng; bob is READ in
// eng; admin is a platform admin. Then bob (a read-role group-mate of the owner) must get the cluster's
// READ policy, alice the WRITE policy, and admin the standing admin policy - nothing more.
func TestDesiredAccessMirrorsAccessTo(t *testing.T) {
	eng := &domain.Group{ID: "g-eng", Name: "Engineering"}
	admin := mkUser("u-admin", "admin", true)
	alice := mkUser("u-alice", "alice", false, domain.GroupMembership{GroupID: eng.ID, Role: domain.GroupRoleWrite})
	bob := mkUser("u-bob", "bob", false, domain.GroupMembership{GroupID: eng.ID, Role: domain.GroupRoleRead})
	c1 := &domain.Cluster{ID: "c1", OwnerID: alice.ID}

	got := DesiredAccess(AccessSnapshot{
		Users:    []*domain.User{admin, alice, bob},
		Groups:   []*domain.Group{eng},
		Clusters: []*domain.Cluster{c1},
	})

	// The owner's entity carries the write policy for the cluster she owns.
	aliceEnt := findEntity(got, EntityName(alice.ID))
	if aliceEnt == nil || !contains(aliceEnt.Policies, PolicyWrite(c1.ID)) {
		t.Fatalf("owner alice's entity missing write policy for c1: %+v", aliceEnt)
	}
	if aliceEnt.Alias != "alice" {
		t.Fatalf("alice entity alias = %q, want alice", aliceEnt.Alias)
	}

	// bob owns nothing, so his entity carries no cluster policies (his access rides the read group).
	bobEnt := findEntity(got, EntityName(bob.ID))
	if bobEnt == nil || len(bobEnt.Policies) != 0 {
		t.Fatalf("non-owner bob's entity should carry no policies: %+v", bobEnt)
	}

	// The write group carries the cluster's WRITE policy and contains only the write-role member.
	gw := findGroup(got, GroupWrite(eng.ID))
	if gw == nil || !contains(gw.Policies, PolicyWrite(c1.ID)) {
		t.Fatalf("eng-write group missing write policy for c1: %+v", gw)
	}
	if !contains(gw.Members, EntityName(alice.ID)) || contains(gw.Members, EntityName(bob.ID)) {
		t.Fatalf("eng-write members wrong: %+v", gw.Members)
	}

	// The read group carries the cluster's READ policy and contains only the read-role member - this is
	// the crux: bob, a read-role group-mate of the owner, can VIEW the cluster's path but not edit it.
	gr := findGroup(got, GroupRead(eng.ID))
	if gr == nil || !contains(gr.Policies, PolicyRead(c1.ID)) {
		t.Fatalf("eng-read group missing read policy for c1: %+v", gr)
	}
	if contains(gr.Policies, PolicyWrite(c1.ID)) {
		t.Fatalf("eng-read group must NOT carry the write policy: %+v", gr.Policies)
	}
	if !contains(gr.Members, EntityName(bob.ID)) {
		t.Fatalf("eng-read should contain bob: %+v", gr.Members)
	}

	// Admins get the standing admin group + policy, never a per-cluster attachment.
	adminG := findGroup(got, GroupAdmins)
	if adminG == nil || !contains(adminG.Policies, PolicyAdmin) || !contains(adminG.Members, EntityName(admin.ID)) {
		t.Fatalf("admins group wrong: %+v", adminG)
	}
}

// TestPoliciesForUser checks the handoff-token policy resolution matches accessTo: owner/admin get
// write/admin, a read-role group-mate gets read, a stranger gets nothing.
func TestPoliciesForUser(t *testing.T) {
	eng := "g-eng"
	alice := mkUser("u-alice", "alice", false, domain.GroupMembership{GroupID: eng, Role: domain.GroupRoleWrite})
	bob := mkUser("u-bob", "bob", false, domain.GroupMembership{GroupID: eng, Role: domain.GroupRoleRead})
	carol := mkUser("u-carol", "carol", false) // no groups
	admin := mkUser("u-admin", "admin", true)
	c1 := &domain.Cluster{ID: "c1", OwnerID: alice.ID}
	owners := map[string]*domain.User{alice.ID: alice}
	clusters := []*domain.Cluster{c1}

	cases := []struct {
		name string
		u    *domain.User
		want []string
	}{
		{"owner→write", alice, []string{PolicyWrite(c1.ID)}},
		{"read-mate→read", bob, []string{PolicyRead(c1.ID)}},
		{"stranger→none", carol, nil},
		{"admin→admin", admin, []string{PolicyAdmin}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PoliciesForUser(tc.u, clusters, owners)
			if len(got) != len(tc.want) {
				t.Fatalf("policies = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("policies = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestFakeClusterLifecycle: the Fake records ensure/release so the reconciler and tests can observe it.
func TestFakeClusterLifecycle(t *testing.T) {
	f := NewFake(nil)
	c := &domain.Cluster{ID: "c1", Name: "dev"}
	if err := f.EnsureCluster(context.Background(), c, ESOAuth{}); err != nil {
		t.Fatal(err)
	}
	if !f.Clusters[c.ID] {
		t.Fatal("EnsureCluster did not record the cluster")
	}
	if err := f.ReleaseCluster(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if f.Clusters[c.ID] {
		t.Fatal("ReleaseCluster did not drop the cluster")
	}
}

// TestUIPath builds the "View in Vault" deep-link under the mount.
func TestUIPath(t *testing.T) {
	s := Settings{UIURL: "https://vault.example/", Mount: "kaas"}
	got := s.UIPath("c1")
	want := "https://vault.example/ui/vault/secrets/kaas/kv/list/clusters/c1/"
	if got != want {
		t.Fatalf("UIPath = %q, want %q", got, want)
	}
}
