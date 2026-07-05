package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/monitoring"
	"github.com/Daniel-Vaz/KaaS-demo/internal/reconcile"
	"github.com/Daniel-Vaz/KaaS-demo/internal/secrets"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// upgradeChainCatalog is a synthetic multi-hop catalog for exercising PromoteCluster's
// reachability checks, where each hop changes exactly one component: 2025.1 (metrics-server
// 3.12.2) → 2025.2 (metrics-server 3.13.1; add-on only) → 2025.3 (1.35.6→1.36.2; Kubernetes only,
// same OS) → 2026.1 (ubuntu-22.04→24.04; OS only). Its head, 2026.1, mirrors the shipped
// catalog's single current bundle exactly - the shipped catalog no longer carries this history
// once a bundle is retired, so this fixture stands in for what it looked like mid-chain (see the
// matching fixture in internal/catalog and internal/reconcile's tests).
func upgradeChainCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Parse([]byte(`{
      "os":[
        {"name":"ubuntu-22.04","family":"ubuntu","release":"22.04","status":"supported"},
        {"name":"ubuntu-24.04","family":"ubuntu","release":"24.04","status":"supported"}
      ],
      "kubernetes":[{"version":"1.35.6","status":"supported"},{"version":"1.36.2","status":"supported"}],
      "addons":[
        {"name":"cilium","type":"cni","version":"1.19.5","status":"supported"},
        {"name":"metrics-server","type":"addon","version":"3.13.1","status":"supported"}
      ],
      "bundles":[
        {"name":"2025.1","status":"supported","os":"ubuntu-22.04","kubernetes":"1.35.6","cni":"cilium","addons":{"cilium":"1.19.5","metrics-server":"3.12.2"},"supersedes":""},
        {"name":"2025.2","status":"supported","os":"ubuntu-22.04","kubernetes":"1.35.6","cni":"cilium","addons":{"cilium":"1.19.5","metrics-server":"3.13.1"},"supersedes":"2025.1"},
        {"name":"2025.3","status":"supported","os":"ubuntu-22.04","kubernetes":"1.36.2","cni":"cilium","addons":{"cilium":"1.19.5","metrics-server":"3.13.1"},"supersedes":"2025.2"},
        {"name":"2026.1","status":"supported","os":"ubuntu-24.04","kubernetes":"1.36.2","cni":"cilium","addons":{"cilium":"1.19.5","metrics-server":"3.13.1"},"supersedes":"2025.3"}
      ]}`))
	if err != nil {
		t.Fatalf("synthetic upgrade-chain catalog invalid: %v", err)
	}
	return c
}

// newPromoteApp builds a minimal App (memory store + the multi-hop test catalog + a fake config
// manager) sufficient to exercise PromoteCluster, which purges any stale control-plane backup via
// Rec.Cfg.
func newPromoteApp(t *testing.T) *App {
	t.Helper()
	return &App{
		Store:   store.NewMemory(),
		Catalog: upgradeChainCatalog(t),
		Rec:     &reconcile.Reconciler{Cfg: config.NewFake()},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// adminActor is a stand-in admin used by the promote tests: authorizeCluster only inspects
// actor.IsAdmin / OwnerID, so it needs no store row.
var adminActor = &domain.User{ID: "admin", Username: "admin", IsAdmin: true}

func readyCluster(id, bundle, os string, controlPlanes int) *domain.Cluster {
	return &domain.Cluster{
		ID: id, Name: id, Size: "small", Phase: domain.PhaseReady, Generation: 1, ObservedGeneration: 1,
		ControlPlanes: controlPlanes, Bundle: bundle, OSImage: os, K8sVersion: "1.35.6",
		CNI: "cilium", CNIVersion: "1.19.5",
	}
}

// TestPromoteKubernetesSingleNode: the user's core scenario - a single-node cluster upgrading
// Kubernetes 1.35.6 → 1.36.2 (2025.1 → 2025.3, same OS) must be allowed, even though the path
// crosses the add-on hop. It records the target + bumps the generation.
func TestPromoteKubernetesSingleNode(t *testing.T) {
	a := newPromoteApp(t)
	_ = a.Store.CreateCluster(readyCluster("k", "2025.1", "ubuntu-22.04", 1))

	c, err := a.PromoteCluster(adminActor, "k", "2025.3")
	if err != nil {
		t.Fatalf("promote (single-node k8s upgrade) rejected: %v", err)
	}
	if c.TargetBundle != "2025.3" || c.Generation != 2 {
		t.Fatalf("after promote: target=%q gen=%d, want 2025.3/2", c.TargetBundle, c.Generation)
	}
}

// TestPromoteAddonOnlySingleNode: an add-on-only hop (2025.1 → 2025.2) is allowed for a single
// control plane - nothing about the nodes changes.
func TestPromoteAddonOnlySingleNode(t *testing.T) {
	a := newPromoteApp(t)
	_ = a.Store.CreateCluster(readyCluster("a", "2025.1", "ubuntu-22.04", 1))

	if _, err := a.PromoteCluster(adminActor, "a", "2025.2"); err != nil {
		t.Fatalf("promote (add-on only, single node) rejected: %v", err)
	}
}

// TestPromoteOSChangeSingleNodeAllowed: the OS hop (2025.3 → 2026.1) on a single control plane is
// now allowed - the reconciler rebuilds it via etcd backup/restore onto the same IP.
func TestPromoteOSChangeSingleNodeAllowed(t *testing.T) {
	a := newPromoteApp(t)
	_ = a.Store.CreateCluster(readyCluster("s", "2025.3", "ubuntu-22.04", 1))

	c, err := a.PromoteCluster(adminActor, "s", "2026.1")
	if err != nil {
		t.Fatalf("promote (single-node OS upgrade) rejected: %v", err)
	}
	if c.TargetBundle != "2026.1" {
		t.Fatalf("target = %q, want 2026.1", c.TargetBundle)
	}
}

// TestPromoteOSChangeHAAllowed: the OS hop (2025.3 → 2026.1) is allowed for an HA control plane.
func TestPromoteOSChangeHAAllowed(t *testing.T) {
	a := newPromoteApp(t)
	_ = a.Store.CreateCluster(readyCluster("h", "2025.3", "ubuntu-22.04", 3))

	c, err := a.PromoteCluster(adminActor, "h", "2026.1")
	if err != nil {
		t.Fatalf("promote (OS change, HA) rejected: %v", err)
	}
	if c.TargetBundle != "2026.1" {
		t.Fatalf("target = %q, want 2026.1", c.TargetBundle)
	}
}

// TestPromoteUnreachableRejected: a bundle not on the cluster's upgrade chain is rejected.
func TestPromoteUnreachableRejected(t *testing.T) {
	a := newPromoteApp(t)
	_ = a.Store.CreateCluster(readyCluster("x", "2026.1", "ubuntu-24.04", 1)) // already the head

	if _, err := a.PromoteCluster(adminActor, "x", "2025.1"); err == nil {
		t.Fatal("expected downgrade/unreachable promotion to be rejected")
	}
}

// --- Monitoring access control ---------------------------------------------

func newMonitoringApp(t *testing.T) *App {
	t.Helper()
	box, err := secrets.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &App{
		Store:   store.NewMemory(),
		Catalog: upgradeChainCatalog(t),
		Secrets: box,
		Monitor: monitoring.NewFake(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestMonitoringGating covers the access/gating checks: not-Ready → ErrClusterNotReady, Ready but no
// stack → ErrMonitoringNotEnabled, and cross-tenant → not-found (a tenant can't probe others).
func TestMonitoringGating(t *testing.T) {
	a := newMonitoringApp(t)
	owner := &domain.User{ID: "u1", Username: "owner"}

	notReady := &domain.Cluster{ID: "c-nr", Name: "nr", OwnerID: owner.ID, Phase: domain.PhaseProvisioningInfra}
	ready := &domain.Cluster{ID: "c-r", Name: "r", OwnerID: owner.ID, Phase: domain.PhaseReady}
	for _, c := range []*domain.Cluster{notReady, ready} {
		if err := a.Store.CreateCluster(c); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := a.Monitoring(context.Background(), owner, "c-nr", "overview", "15m"); !errors.Is(err, ErrClusterNotReady) {
		t.Fatalf("not-ready err = %v, want ErrClusterNotReady", err)
	}
	if _, err := a.Monitoring(context.Background(), owner, "c-r", "overview", "15m"); !errors.Is(err, ErrMonitoringNotEnabled) {
		t.Fatalf("no-stack err = %v, want ErrMonitoringNotEnabled", err)
	}
	// A different tenant can't see the cluster at all.
	other := &domain.User{ID: "u2", Username: "other"}
	if _, err := a.Monitoring(context.Background(), other, "c-r", "overview", "15m"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant err = %v, want store.ErrNotFound", err)
	}
}

// TestMonitoringHappyPath: a Ready cluster with the stack installed resolves a tab through the fake
// querier using the admin kubeconfig.
func TestMonitoringHappyPath(t *testing.T) {
	a := newMonitoringApp(t)
	owner := &domain.User{ID: "u1", Username: "owner"}
	c := &domain.Cluster{
		ID: "c1", Name: "demo", OwnerID: owner.ID, Phase: domain.PhaseReady,
		Addons: []domain.Addon{{Name: monitoring.AddonName, Phase: "installed"}},
	}
	if err := a.Store.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	ct, err := a.Secrets.Seal([]byte("kubeconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.SaveSecret(c.ID, domain.SecretKubeconfig, ct); err != nil {
		t.Fatal(err)
	}
	data, err := a.Monitoring(context.Background(), owner, "c1", "overview", "15m")
	if err != nil {
		t.Fatalf("Monitoring: %v", err)
	}
	if data.Tab != "overview" || len(data.Panels) == 0 {
		t.Fatalf("tab data = %+v, want overview with panels", data)
	}
	// An unknown tab is a not-found error.
	if _, err := a.Monitoring(context.Background(), owner, "c1", "nope", "15m"); !errors.Is(err, monitoring.ErrUnknownTab) {
		t.Fatalf("unknown tab err = %v, want ErrUnknownTab", err)
	}
}
