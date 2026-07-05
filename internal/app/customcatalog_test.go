package app

import (
	"errors"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// sampleAddon is a valid custom add-on definition used across the tests.
func sampleAddon(name string) domain.CustomAddon {
	return domain.CustomAddon{
		Name:        name,
		Description: "a demo chart",
		Repo:        "https://example.com/charts",
		Chart:       "podinfo",
		Version:     "6.5.0",
		Values:      "replicaCount: 2\n",
	}
}

// TestCustomCatalogCRUD: an owner creates a catalog, adds/updates/removes an add-on, and the
// (owner, name) uniqueness + add-on validation are enforced.
func TestCustomCatalogCRUD(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")

	cc, err := a.CreateCustomCatalog(alice, "team-charts")
	if err != nil {
		t.Fatalf("create catalog: %v", err)
	}
	if _, err := a.CreateCustomCatalog(alice, "team-charts"); err == nil {
		t.Fatal("duplicate catalog name should be rejected")
	}

	if _, err := a.AddCustomAddon(alice, cc.ID, sampleAddon("podinfo")); err != nil {
		t.Fatalf("add addon: %v", err)
	}
	if _, err := a.AddCustomAddon(alice, cc.ID, sampleAddon("podinfo")); err == nil {
		t.Fatal("duplicate add-on name should be rejected")
	}
	// Invalid: uppercase name is not a DNS label.
	if _, err := a.AddCustomAddon(alice, cc.ID, sampleAddon("PodInfo")); err == nil {
		t.Fatal("non-DNS-label add-on name should be rejected")
	}
	// Invalid: classic chart with no repo.
	bad := sampleAddon("nginx")
	bad.Repo = ""
	if _, err := a.AddCustomAddon(alice, cc.ID, bad); err == nil {
		t.Fatal("classic chart without repo should be rejected")
	}

	updated, err := a.UpdateCustomAddon(alice, cc.ID, "podinfo", func() domain.CustomAddon {
		ad := sampleAddon("podinfo")
		ad.Version = "6.6.0"
		return ad
	}())
	if err != nil {
		t.Fatalf("update addon: %v", err)
	}
	if updated.Addons[0].Version != "6.6.0" {
		t.Fatalf("addon version = %q, want 6.6.0", updated.Addons[0].Version)
	}

	got, err := a.GetCustomCatalog(alice, cc.ID)
	if err != nil {
		t.Fatalf("get catalog: %v", err)
	}
	if got.OwnerUsername != "alice" || got.Access != "edit" || len(got.Addons) != 1 {
		t.Fatalf("catalog view = %+v", got)
	}

	if _, err := a.RemoveCustomAddon(alice, cc.ID, "podinfo"); err != nil {
		t.Fatalf("remove addon: %v", err)
	}
	if err := a.DeleteCustomCatalog(alice, cc.ID); err != nil {
		t.Fatalf("delete catalog: %v", err)
	}
	list, _ := a.ListCustomCatalogs(alice)
	if len(list) != 0 {
		t.Fatalf("expected no catalogs after delete, got %d", len(list))
	}
}

// TestCustomCatalogGroupSharing: a catalog is shared through the group model exactly like clusters -
// a read-role group-mate can view but not edit, a write-role group-mate can edit, and an ungrouped
// user can't see it at all.
func TestCustomCatalogGroupSharing(t *testing.T) {
	a := newTenancyApp(t)
	ad := admin(t, a)
	alice, _ := a.Register("alice", "password")
	bob, _ := a.Register("bob", "password")
	carol, _ := a.Register("carol", "password")
	dave, _ := a.Register("dave", "password")
	g, _ := a.CreateGroup(ad, "team")
	bob = assignGroup(t, a, bob.ID, g.ID, domain.GroupRoleRead)
	carol = assignGroup(t, a, carol.ID, g.ID, domain.GroupRoleWrite)
	alice = assignGroup(t, a, alice.ID, g.ID, domain.GroupRoleWrite)

	cc, _ := a.CreateCustomCatalog(alice, "team-charts")

	// Read-role bob sees it as view-only and cannot edit.
	list, _ := a.ListCustomCatalogs(bob)
	if len(list) != 1 || list[0].Access != "view" {
		t.Fatalf("bob's view of alice's catalog = %+v", list)
	}
	if _, err := a.AddCustomAddon(bob, cc.ID, sampleAddon("podinfo")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob (read role) AddCustomAddon = %v, want ErrForbidden", err)
	}

	// Write-role carol can edit.
	if _, err := a.AddCustomAddon(carol, cc.ID, sampleAddon("podinfo")); err != nil {
		t.Fatalf("carol (write role) AddCustomAddon: %v", err)
	}

	// Ungrouped dave can't see it (store.ErrNotFound, not ErrForbidden - no existence leak).
	if _, err := a.GetCustomCatalog(dave, cc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dave's GetCustomCatalog = %v, want store.ErrNotFound", err)
	}
	if daveList, _ := a.ListCustomCatalogs(dave); len(daveList) != 0 {
		t.Fatalf("dave should see no catalogs, got %d", len(daveList))
	}
}

// TestResolveCustomAddonsOntoCluster: selecting a custom add-on at create time copies its chart
// definition onto the cluster (self-contained), and a name collision with a built-in add-on is
// rejected.
func TestResolveCustomAddonsOntoCluster(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	alice = grantQuota(t, a, alice.ID, 8, 16384)
	cc, _ := a.CreateCustomCatalog(alice, "team-charts")
	_, _ = a.AddCustomAddon(alice, cc.ID, sampleAddon("podinfo"))

	c, err := a.CreateCluster(alice, CreateRequest{
		Name:         "dev",
		Size:         "small",
		CustomAddons: []domain.CustomAddonRef{{CatalogID: cc.ID, Name: "podinfo"}},
	})
	if err != nil {
		t.Fatalf("create cluster with custom add-on: %v", err)
	}
	var found *domain.Addon
	for i := range c.Addons {
		if c.Addons[i].Name == "podinfo" {
			found = &c.Addons[i]
		}
	}
	if found == nil {
		t.Fatalf("podinfo not on cluster; addons=%+v", c.Addons)
	}
	if !found.Custom() || found.CatalogID != cc.ID || found.Chart != "podinfo" ||
		found.Repo != "https://example.com/charts" || found.ValuesOverride != "replicaCount: 2\n" {
		t.Fatalf("custom add-on not self-contained: %+v", found)
	}

	// A custom add-on whose name collides with a built-in add-on is rejected.
	_, _ = a.AddCustomAddon(alice, cc.ID, func() domain.CustomAddon {
		x := sampleAddon("metrics-server")
		return x
	}())
	if _, err := a.CreateCluster(alice, CreateRequest{
		Name:         "dev2",
		Size:         "small",
		CustomAddons: []domain.CustomAddonRef{{CatalogID: cc.ID, Name: "metrics-server"}},
	}); err == nil {
		t.Fatal("custom add-on colliding with a built-in add-on name should be rejected")
	}
}

// TestCustomAddonPartitionIsolation: reconciling the built-in add-on set leaves custom add-ons
// untouched, and vice-versa - the two partitions are independent.
func TestCustomAddonPartitionIsolation(t *testing.T) {
	a := newTenancyApp(t)
	alice, _ := a.Register("alice", "password")
	alice = grantQuota(t, a, alice.ID, 8, 16384)
	cc, _ := a.CreateCustomCatalog(alice, "team-charts")
	_, _ = a.AddCustomAddon(alice, cc.ID, sampleAddon("podinfo"))

	c := readyCluster("dev", "2026.1", "ubuntu-24.04", 1)
	c.OwnerID = alice.ID
	c.Addons = []domain.Addon{
		{Name: "metrics-server", Version: "3.13.1", Phase: "installed"},
		{Name: "podinfo", Version: "6.5.0", Phase: "installed", CatalogID: cc.ID, Chart: "podinfo", Repo: "https://example.com/charts"},
	}
	_ = a.Store.CreateCluster(c)

	// Built-in-only edit (drop metrics-server); custom add-on must be untouched.
	empty := []string{}
	if _, err := a.UpdateCluster(alice, "dev", UpdateRequest{Addons: &empty}); err != nil {
		t.Fatalf("built-in edit: %v", err)
	}
	reloaded, _ := a.Store.GetCluster("dev")
	if podinfo := findAddon(reloaded, "podinfo"); podinfo == nil || podinfo.Phase == "removing" {
		t.Fatalf("custom add-on disturbed by built-in edit: %+v", reloaded.Addons)
	}
	if ms := findAddon(reloaded, "metrics-server"); ms == nil || ms.Phase != "removing" {
		t.Fatalf("built-in metrics-server should be marked removing: %+v", reloaded.Addons)
	}

	// Custom-only edit (drop podinfo); built-in must be untouched.
	noCustom := []domain.CustomAddonRef{}
	if _, err := a.UpdateCluster(alice, "dev", UpdateRequest{CustomAddons: &noCustom}); err != nil {
		t.Fatalf("custom edit: %v", err)
	}
	reloaded, _ = a.Store.GetCluster("dev")
	if podinfo := findAddon(reloaded, "podinfo"); podinfo == nil || podinfo.Phase != "removing" {
		t.Fatalf("custom podinfo should be marked removing: %+v", reloaded.Addons)
	}
}

func findAddon(c *domain.Cluster, name string) *domain.Addon {
	for i := range c.Addons {
		if c.Addons[i].Name == name {
			return &c.Addons[i]
		}
	}
	return nil
}
