package app

import (
	"errors"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Profile is the one place a non-admin resolves their own group IDs to names, and it must do so
// without the admin-only ListGroups. It also carries the per-provider quota the account page shows.
func TestProfileResolvesOwnGroupsAndQuota(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	reads, _ := a.CreateGroup(ad, "team-reads")
	writes, _ := a.CreateGroup(ad, "team-writes")
	alice = grantQuota(t, a, alice.ID, 4, 8192)

	mems := []domain.GroupMembership{
		{GroupID: writes.ID, Role: domain.GroupRoleWrite},
		{GroupID: reads.ID, Role: domain.GroupRoleRead},
	}
	alice, err := a.UpdateUser(ad, alice.ID, UpdateUserRequest{Memberships: &mems})
	if err != nil {
		t.Fatalf("assign groups: %v", err)
	}
	if _, err := a.CreateCluster(alice, CreateRequest{Name: "dev", Size: "small"}); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	rep, err := a.Profile(alice)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if rep.User.Username != "alice" {
		t.Fatalf("Profile user = %q, want alice", rep.User.Username)
	}
	// Sorted by name, each carrying the role held in THAT group - a user is Read in one and Write
	// in another at the same time. Source is empty on an admin-created group (the "" means local"
	// convention, which the portal's directoryManaged mirrors).
	want := []ProfileGroup{
		{ID: reads.ID, Name: "team-reads", Role: domain.GroupRoleRead},
		{ID: writes.ID, Name: "team-writes", Role: domain.GroupRoleWrite},
	}
	if len(rep.Groups) != len(want) {
		t.Fatalf("Profile groups = %+v, want %+v", rep.Groups, want)
	}
	for i, w := range want {
		if rep.Groups[i] != w {
			t.Fatalf("Profile groups[%d] = %+v, want %+v", i, rep.Groups[i], w)
		}
	}

	if len(rep.Capacity.Providers) == 0 {
		t.Fatal("Profile capacity has no providers")
	}
	if rep.Capacity.TotalVCPU != 4 || rep.Capacity.UsedVCPU == 0 {
		t.Fatalf("Profile capacity = %+v, want the grant of 4 vCPU and non-zero usage", rep.Capacity)
	}
}

func TestProfileRequiresActor(t *testing.T) {
	a := newTenancyApp(t)
	if _, err := a.Profile(nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Profile(nil) = %v, want ErrForbidden", err)
	}
}
