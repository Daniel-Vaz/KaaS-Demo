package store

import (
	"errors"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func TestMemoryUserCRUD(t *testing.T) {
	m := NewMemory()
	u := &domain.User{ID: "u1", Username: "alice", Quotas: map[string]domain.ResourceQuota{"kvm": {VCPU: 4}}}
	if err := m.CreateUser(u); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := m.GetUser("u1")
	if err != nil || got.Username != "alice" {
		t.Fatalf("get: %v %+v", err, got)
	}
	byName, err := m.GetUserByUsername("alice")
	if err != nil || byName.ID != "u1" {
		t.Fatalf("get by username: %v %+v", err, byName)
	}

	u.Quotas["kvm"] = domain.ResourceQuota{VCPU: 8}
	if err := m.UpdateUser(u); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = m.GetUser("u1")
	if got.QuotaOn("kvm").VCPU != 8 {
		t.Fatalf("kvm quota after update = %d, want 8", got.QuotaOn("kvm").VCPU)
	}
	// The store must not share the Quotas map with its callers: mutating what a caller was handed
	// cannot be allowed to rewrite the stored grant behind an admission check's back.
	got.Quotas["kvm"] = domain.ResourceQuota{VCPU: 999}
	again, _ := m.GetUser("u1")
	if again.QuotaOn("kvm").VCPU != 8 {
		t.Fatalf("stored kvm quota = %d after a caller mutated its copy, want 8 - the map is shared", again.QuotaOn("kvm").VCPU)
	}

	users, _ := m.ListUsers()
	if len(users) != 1 {
		t.Fatalf("list len = %d, want 1", len(users))
	}

	if err := m.DeleteUser("u1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.GetUser("u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
}

func TestMemoryUsernameUnique(t *testing.T) {
	m := NewMemory()
	if err := m.CreateUser(&domain.User{ID: "u1", Username: "alice"}); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := m.CreateUser(&domain.User{ID: "u2", Username: "alice"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username = %v, want ErrConflict", err)
	}
	// Renaming u2 onto an existing username also conflicts.
	_ = m.CreateUser(&domain.User{ID: "u2", Username: "bob"})
	if err := m.UpdateUser(&domain.User{ID: "u2", Username: "alice"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename onto taken username = %v, want ErrConflict", err)
	}
}

func TestMemoryGroupCRUD(t *testing.T) {
	m := NewMemory()
	g := &domain.Group{ID: "g1", Name: "team-a"}
	if err := m.CreateGroup(g); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := m.GetGroup("g1")
	if err != nil || got.Name != "team-a" {
		t.Fatalf("get: %v %+v", err, got)
	}

	g.Name = "team-alpha"
	if err := m.UpdateGroup(g); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = m.GetGroup("g1")
	if got.Name != "team-alpha" {
		t.Fatalf("name after update = %q, want team-alpha", got.Name)
	}

	groups, _ := m.ListGroups()
	if len(groups) != 1 {
		t.Fatalf("list len = %d, want 1", len(groups))
	}

	if err := m.DeleteGroup("g1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.GetGroup("g1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
}

func TestMemoryGroupNameUnique(t *testing.T) {
	m := NewMemory()
	if err := m.CreateGroup(&domain.Group{ID: "g1", Name: "team-a"}); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := m.CreateGroup(&domain.Group{ID: "g2", Name: "team-a"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate group name = %v, want ErrConflict", err)
	}
	_ = m.CreateGroup(&domain.Group{ID: "g2", Name: "team-b"})
	if err := m.UpdateGroup(&domain.Group{ID: "g2", Name: "team-a"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename onto taken name = %v, want ErrConflict", err)
	}
}

func TestMemoryListClustersByOwner(t *testing.T) {
	m := NewMemory()
	_ = m.CreateCluster(&domain.Cluster{ID: "c1", Name: "a", OwnerID: "alice"})
	_ = m.CreateCluster(&domain.Cluster{ID: "c2", Name: "b", OwnerID: "alice"})
	_ = m.CreateCluster(&domain.Cluster{ID: "c3", Name: "c", OwnerID: "bob"})

	alice, _ := m.ListClustersByOwner("alice")
	if len(alice) != 2 {
		t.Fatalf("alice owns %d clusters, want 2", len(alice))
	}
	bob, _ := m.ListClustersByOwner("bob")
	if len(bob) != 1 {
		t.Fatalf("bob owns %d clusters, want 1", len(bob))
	}
	none, _ := m.ListClustersByOwner("carol")
	if len(none) != 0 {
		t.Fatalf("carol owns %d clusters, want 0", len(none))
	}
}
