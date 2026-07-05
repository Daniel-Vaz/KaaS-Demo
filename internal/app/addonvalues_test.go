package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/addons/values"
	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

func newAddonValuesApp(t *testing.T) *App {
	t.Helper()
	cat, err := catalog.Default()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return &App{
		Store:   store.NewMemory(),
		Catalog: cat,
		Values:  values.NewFake(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestSetClusterAddonValues: editing an installed add-on's values flips it to "updating", bumps the
// generation, records an operation, and persists the override; resetting to empty is the inverse.
func TestSetClusterAddonValues(t *testing.T) {
	a := newAddonValuesApp(t)
	c := readyCluster("c", "2026.1", "ubuntu-24.04", 1)
	c.OwnerID = adminActor.ID
	c.Addons = []domain.Addon{{Name: "trivy-operator", Version: "0.34.0", Phase: "installed"}}
	_ = a.Store.CreateCluster(c)

	got, err := a.SetClusterAddonValues(adminActor, "c", "trivy-operator", "trivy:\n  ignoreUnfixed: true\n")
	if err != nil {
		t.Fatalf("SetClusterAddonValues: %v", err)
	}
	if got.Generation != 2 {
		t.Fatalf("generation = %d, want 2", got.Generation)
	}
	ad := got.Addons[0]
	if ad.Phase != "updating" || !strings.Contains(ad.ValuesOverride, "ignoreUnfixed: true") {
		t.Fatalf("addon not updated: phase=%q override=%q", ad.Phase, ad.ValuesOverride)
	}
	ops, _ := a.Store.ListOperations("c")
	if len(ops) == 0 || ops[0].Kind != domain.OpAddons {
		t.Fatalf("expected an addons operation, got %+v", ops)
	}

	// Reset to defaults.
	reset, err := a.SetClusterAddonValues(adminActor, "c", "trivy-operator", "")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset.Generation != 3 || reset.Addons[0].ValuesOverride != "" || reset.Addons[0].Phase != "updating" {
		t.Fatalf("reset failed: gen=%d override=%q phase=%q",
			reset.Generation, reset.Addons[0].ValuesOverride, reset.Addons[0].Phase)
	}
}

// TestSetClusterAddonValuesRejectsBadYAML: a malformed override is rejected before any state change.
func TestSetClusterAddonValuesRejectsBadYAML(t *testing.T) {
	a := newAddonValuesApp(t)
	c := readyCluster("c", "2026.1", "ubuntu-24.04", 1)
	c.OwnerID = adminActor.ID
	c.Addons = []domain.Addon{{Name: "trivy-operator", Version: "0.34.0", Phase: "installed"}}
	_ = a.Store.CreateCluster(c)

	if _, err := a.SetClusterAddonValues(adminActor, "c", "trivy-operator", "a: b\n  c: d\n"); err == nil {
		t.Fatalf("expected malformed YAML to be rejected")
	}
	reloaded, _ := a.Store.GetCluster("c")
	if reloaded.Generation != 1 || reloaded.Addons[0].Phase != "installed" {
		t.Fatalf("state changed on rejected edit: gen=%d phase=%q", reloaded.Generation, reloaded.Addons[0].Phase)
	}
}

// TestResolveAddonsBundledFirst: when a user picks a mix of the bundle's platform add-ons and
// optional catalog add-ons, resolveAddons orders every bundled add-on ahead of every optional one
// (so kube-prometheus-stack's ServiceMonitor CRDs exist before an optional add-on that publishes
// into them). Within each group it stays (Priority, Name) - kube-prometheus-stack's -100 keeps it
// the very first platform add-on. Requested here in a deliberately hostile order.
func TestResolveAddonsBundledFirst(t *testing.T) {
	a := newAddonValuesApp(t)
	rb, err := a.Catalog.Resolve("2026.1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	requested := []string{"cert-manager", "metrics-server", "external-secrets", "argocd", "trivy-operator", "kube-prometheus-stack"}
	out, err := a.resolveAddons(rb, requested, nil)
	if err != nil {
		t.Fatalf("resolveAddons: %v", err)
	}
	got := make([]string, len(out))
	for i, ad := range out {
		got[i] = ad.Name
	}
	// Bundled first, in (Priority, Name) order - kube-prometheus-stack (-100), cert-manager (-8, up
	// early so its issuer CRDs exist for the gateway wiring), external-secrets (-3, so its CRDs exist
	// for the Vault wiring), then metrics-server, trivy-operator (0, by name) - then the optional argocd.
	want := []string{"kube-prometheus-stack", "cert-manager", "external-secrets", "metrics-server", "trivy-operator", "argocd"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("install order = %v, want %v", got, want)
	}
}

// TestAddonValuesSeed: the wizard payload merges chart defaults with catalog overrides.
func TestAddonValuesSeed(t *testing.T) {
	a := newAddonValuesApp(t)
	view, err := a.AddonValues(context.Background(), adminActor, "2026.1", "metrics-server")
	if err != nil {
		t.Fatalf("AddonValues: %v", err)
	}
	if view.Version != "3.13.1" {
		t.Fatalf("version = %q, want the bundle-pinned 3.13.1", view.Version)
	}
	if !strings.Contains(view.EffectiveValues, "kubelet-insecure-tls") {
		t.Fatalf("effective values missing catalog override:\n%s", view.EffectiveValues)
	}
}
