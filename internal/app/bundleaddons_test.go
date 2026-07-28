package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// bundleAddonNames is the add-ons a bundle ships minus its CNI - the set the create-time lock
// governs (the CNI is installed at bootstrap, never as a selectable add-on).
func bundleAddonNames(t *testing.T, a *App, bundle string) []string {
	t.Helper()
	rb, err := a.Catalog.Resolve(bundle)
	if err != nil {
		t.Fatalf("resolve %s: %v", bundle, err)
	}
	names := make([]string, 0, len(rb.Addons))
	for _, ad := range rb.Addons {
		names = append(names, ad.Name)
	}
	return names
}

func addonNames(c *domain.Cluster) []string {
	out := make([]string, 0, len(c.Addons))
	for _, ad := range c.Addons {
		out = append(out, ad.Name)
	}
	return out
}

// Default deployment: the bundle's add-ons are what a cluster is born with, and a create request
// that drops one is refused - it is an edit to make on a Ready cluster, not at admission. This is
// the API-side half of the wizard's locked cards; the portal renders the lock, the server holds it.
func TestBundleAddonsLockedAtCreateByDefault(t *testing.T) {
	a, alice := newPoolApp(t)
	bundled := bundleAddonNames(t, a, "2026.1")
	if len(bundled) < 2 {
		t.Fatalf("bundle carries %d add-ons, this test needs a few", len(bundled))
	}

	// Dropping one of them is refused, and the error names it.
	dropped := bundled[0]
	_, err := a.CreateCluster(alice, CreateRequest{Name: "lean", Size: "small", Addons: bundled[1:]})
	if err == nil {
		t.Fatal("dropping a bundle add-on at create time should be refused while the lock is on")
	}
	if !strings.Contains(err.Error(), dropped) {
		t.Errorf("error %q should name the add-on that was dropped (%s)", err, dropped)
	}

	// So is asking for none at all - an empty (but present) selection is a real "none", not a
	// silent fall-back to the bundle's own set.
	if _, err := a.CreateCluster(alice, CreateRequest{Name: "bare", Size: "small", Addons: []string{}}); err == nil {
		t.Fatal("an explicitly empty selection should be refused while the lock is on")
	}

	// Omitting the field entirely still means "the bundle's add-ons".
	c, err := a.CreateCluster(alice, CreateRequest{Name: "default", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range bundled {
		if !slices.Contains(addonNames(c), want) {
			t.Errorf("cluster is missing bundled add-on %q (got %v)", want, addonNames(c))
		}
	}

	// The lock is admission-only: the same cluster can drop them once it exists.
	none := []string{}
	got, err := a.UpdateCluster(alice, c.ID, UpdateRequest{Addons: &none})
	if err != nil {
		t.Fatalf("removing add-ons from an existing cluster must stay allowed: %v", err)
	}
	for _, ad := range got.Addons {
		if ad.Phase != "removing" {
			t.Errorf("add-on %s phase = %q, want it being removed", ad.Name, ad.Phase)
		}
	}
}

// KAAS_BUNDLE_ADDONS_OPTIONAL: with the lock lifted a cluster can be created with a subset of the
// bundle - or with none of it - which is what makes the batteries-included bundle survivable on a
// small KVM host. The bundle still pins the version of whatever is kept.
func TestBundleAddonsOptionalAllowsDeselectionAtCreate(t *testing.T) {
	a, alice := newPoolApp(t)
	a.BundleAddonsOptional = true
	bundled := bundleAddonNames(t, a, "2026.1")

	keep := bundled[0]
	c, err := a.CreateCluster(alice, CreateRequest{Name: "lean", Size: "small", Addons: []string{keep}})
	if err != nil {
		t.Fatalf("deselecting bundle add-ons should be allowed: %v", err)
	}
	if got := addonNames(c); len(got) != 1 || got[0] != keep {
		t.Fatalf("add-ons = %v, want only %q", got, keep)
	}
	b, _ := a.Catalog.Bundle("2026.1")
	if c.Addons[0].Version != b.Addons[keep] {
		t.Errorf("version = %q, want the bundle's pin %q", c.Addons[0].Version, b.Addons[keep])
	}

	// None at all: an empty selection is honoured rather than read as "unspecified".
	bare, err := a.CreateCluster(alice, CreateRequest{Name: "bare", Size: "small", Addons: []string{}})
	if err != nil {
		t.Fatalf("an empty selection should be allowed with the lock lifted: %v", err)
	}
	if len(bare.Addons) != 0 {
		t.Fatalf("add-ons = %v, want none", addonNames(bare))
	}
	// Dropping longhorn drops the storage disks it backs - the per-worker disk is provisioned only
	// when the add-on that consumes it is on the cluster, so the cluster isn't charged for it.
	if len(bare.NodeDisks) != 0 || bare.StorageDiskGB != 0 {
		t.Errorf("NodeDisks = %+v / storage_disk_gb = %d, want none without longhorn", bare.NodeDisks, bare.StorageDiskGB)
	}
	// Omitting the field is still the batteries-included default, flag or no flag.
	full, err := a.CreateCluster(alice, CreateRequest{Name: "full", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Addons) != len(bundled) {
		t.Errorf("add-ons = %v, want the bundle's %v", addonNames(full), bundled)
	}
}
