package reconcile

import (
	"context"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault"
)

// vaultRecorder records what the reconciler provisions and releases, in order (sharing the
// provisioner's destroy log where wired), so the tests can assert the SEQUENCE - a cluster's Vault
// path must be gone before its infrastructure is.
type vaultRecorder struct {
	events  []string
	destroy *[]string
}

func (v *vaultRecorder) EnsurePlatform(context.Context) error { return nil }

func (v *vaultRecorder) EnsureCluster(_ context.Context, c *domain.Cluster, _ vault.ESOAuth) error {
	v.log("ensure " + c.ID)
	return nil
}

func (v *vaultRecorder) ReleaseCluster(_ context.Context, c *domain.Cluster) error {
	v.log("release " + c.ID)
	return nil
}

func (v *vaultRecorder) SyncAccess(context.Context, vault.AccessSnapshot) error { return nil }

func (v *vaultRecorder) MintUserToken(context.Context, []string, map[string]string) (string, error) {
	return "t", nil
}

func (v *vaultRecorder) log(s string) {
	v.events = append(v.events, s)
	if v.destroy != nil {
		*v.destroy = append(*v.destroy, s)
	}
}

// vaultCluster is a cluster with the external-secrets add-on, so the Vault wiring gate is satisfied.
func vaultCluster(id, name string) *domain.Cluster {
	return &domain.Cluster{
		ID: id, Name: name, K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
		Addons: []domain.Addon{
			{Name: "external-secrets", Version: "2.7.0", Phase: "pending"},
		},
	}
}

// TestVaultWiredOnceAfterAddon: a cluster with external-secrets provisions its Vault path exactly once,
// and the VaultWired marker records it so later ticks don't re-provision.
func TestVaultWiredOnceAfterAddon(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &vaultRecorder{}
	r.Vault = rec
	if err := st.CreateCluster(vaultCluster("v1", "dev")); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "v1")

	if len(rec.events) != 1 || rec.events[0] != "ensure v1" {
		t.Fatalf("vault events = %v, want exactly [ensure v1]", rec.events)
	}
	got, _ := st.GetCluster("v1")
	if !got.VaultWired {
		t.Fatal("VaultWired not set - the path would be re-provisioned every tick")
	}
}

// TestVaultNotWiredWithoutAddon: a cluster that deselected external-secrets gets no Vault path (the
// wiring gates on the add-on, so the reservation is simply inert).
func TestVaultNotWiredWithoutAddon(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &vaultRecorder{}
	r.Vault = rec
	c := vaultCluster("v2", "noeso")
	c.Addons = nil
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "v2")
	if len(rec.events) != 0 {
		t.Fatalf("vault events = %v, want none without external-secrets", rec.events)
	}
}

// TestVaultReleasedBeforeDestroy is the invariant that matters most: the cluster's Vault path must be
// torn down BEFORE the infrastructure goes away - a leftover path (its secrets) would outlive the
// cluster that owned it. Same ordering contract as releaseDNS.
func TestVaultReleasedBeforeDestroy(t *testing.T) {
	r, st := newTestReconciler(t)
	var log []string
	rec := &vaultRecorder{destroy: &log}
	r.Vault = rec
	r.Prov = &destroyRecorder{Fake: provision.NewFake(), log: &log}

	if err := st.CreateCluster(vaultCluster("v3", "doomed")); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "v3")

	c, _ := st.GetCluster("v3")
	c.Phase = domain.PhaseDeleting
	c.Generation++
	if err := st.UpdateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "v3")

	if len(log) != 3 || log[0] != "ensure v3" || log[1] != "release v3" || log[2] != "destroy v3" {
		t.Fatalf("sequence = %v, want ensure → release → destroy", log)
	}
}
