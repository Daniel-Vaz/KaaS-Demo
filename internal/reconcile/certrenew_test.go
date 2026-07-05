package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// certCfg is a config.Manager that lets a test drive automatic certificate rotation: it controls the
// expiry CertExpiry observes and the expiry RenewCerts issues, and counts both seam calls. Every other
// method comes from the embedded config.Fake.
type certCfg struct {
	config.Fake
	expiry      time.Time // what CertExpiry returns (the backfill observation)
	renewExpiry time.Time // what RenewCerts returns as the new expiry
	expiryCalls int
	renewCalls  int
}

func (c *certCfg) CertExpiry(context.Context, *domain.Cluster) (time.Time, error) {
	c.expiryCalls++
	return c.expiry, nil
}

func (c *certCfg) RenewCerts(context.Context, *domain.Cluster, time.Time) ([]byte, time.Time, error) {
	c.renewCalls++
	return []byte("renewed-admin-kubeconfig"), c.renewExpiry, nil
}

// readyCluster creates a cluster and drives it to Ready/converged with the fake config, returning the
// fresh Ready row.
func readyCluster(t *testing.T, r *Reconciler, st store.Store, id string) *domain.Cluster {
	t.Helper()
	c := &domain.Cluster{ID: id, Name: id, K8sVersion: "1.36.2", Size: "small", Phase: domain.PhasePending, Generation: 1}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, id)
	got, _ := st.GetCluster(id)
	if got.Phase != domain.PhaseReady {
		t.Fatalf("setup: phase = %s, want Ready", got.Phase)
	}
	return got
}

// TestCertRenewalWhenDue: a Ready cluster whose observed expiry falls within the renewal window is
// surfaced as due, promoted to RenewingCerts, renewed (kubeconfig re-sealed, expiry advanced), and
// then no longer due.
func TestCertRenewalWhenDue(t *testing.T) {
	r, st := newTestReconciler(t)
	r.CertRenewWindow = 30 * 24 * time.Hour
	cfg := &certCfg{renewExpiry: time.Now().AddDate(1, 0, 0)}
	r.Cfg = cfg

	c := readyCluster(t, r, st, "cr1")
	soon := time.Now().Add(10 * 24 * time.Hour) // inside the 30d window
	c.CertNotAfter = &soon
	if err := st.UpdateCluster(c); err != nil {
		t.Fatal(err)
	}

	// Surfaced in both the dedicated query and the unioned work set.
	due, err := r.Store.ClustersDueCertRenewal(r.certRenewCutoff())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != "cr1" {
		t.Fatalf("ClustersDueCertRenewal = %v, want [cr1]", due)
	}
	if work, _ := r.clustersNeedingWork(); len(work) != 1 || work[0].ID != "cr1" {
		t.Fatalf("clustersNeedingWork = %v, want [cr1]", work)
	}

	// Tick 1: Ready -> RenewingCerts (no renew yet).
	step(t, r, st, "cr1")
	if got, _ := st.GetCluster("cr1"); got.Phase != domain.PhaseRenewingCerts {
		t.Fatalf("after tick 1: phase = %s, want RenewingCerts", got.Phase)
	}
	if cfg.renewCalls != 0 {
		t.Fatalf("RenewCerts called before the renew phase (%d)", cfg.renewCalls)
	}

	// Tick 2: RenewingCerts -> Ready, certs renewed.
	step(t, r, st, "cr1")
	got, _ := st.GetCluster("cr1")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("after tick 2: phase = %s, want Ready", got.Phase)
	}
	if cfg.renewCalls != 1 {
		t.Fatalf("RenewCerts calls = %d, want 1", cfg.renewCalls)
	}
	if got.CertNotAfter == nil || got.CertNotAfter.Before(time.Now().Add(300*24*time.Hour)) {
		t.Fatalf("CertNotAfter not advanced past the window: %v", got.CertNotAfter)
	}
	kc, err := r.getSecret("cr1", domain.SecretKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if string(kc) != "renewed-admin-kubeconfig" {
		t.Fatalf("kubeconfig not re-sealed after renewal, got %q", kc)
	}
	if again, _ := r.Store.ClustersDueCertRenewal(r.certRenewCutoff()); len(again) != 0 {
		t.Fatalf("still due after renewal: %v", again)
	}
}

// TestCertBackfillNotDue: a cluster whose expiry has never been observed (CertNotAfter nil) is
// surfaced as due, observed once, stamped, and - when the observed expiry is comfortably far out -
// left Ready without renewing.
func TestCertBackfillNotDue(t *testing.T) {
	r, st := newTestReconciler(t)
	r.CertRenewWindow = 30 * 24 * time.Hour
	cfg := &certCfg{expiry: time.Now().AddDate(1, 0, 0)}
	r.Cfg = cfg

	c := readyCluster(t, r, st, "cb1")
	if c.CertNotAfter != nil {
		t.Fatalf("setup: expected unobserved expiry, got %v", c.CertNotAfter)
	}
	// nil expiry qualifies as due (needs observing).
	if due, _ := r.Store.ClustersDueCertRenewal(r.certRenewCutoff()); len(due) != 1 {
		t.Fatalf("unobserved cluster not surfaced as due: %v", due)
	}

	step(t, r, st, "cb1")
	got, _ := st.GetCluster("cb1")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready (not due)", got.Phase)
	}
	if cfg.expiryCalls != 1 {
		t.Fatalf("CertExpiry calls = %d, want 1", cfg.expiryCalls)
	}
	if cfg.renewCalls != 0 {
		t.Fatalf("RenewCerts calls = %d, want 0 (not due)", cfg.renewCalls)
	}
	if got.CertNotAfter == nil {
		t.Fatal("CertNotAfter not stamped by backfill")
	}
	if again, _ := r.Store.ClustersDueCertRenewal(r.certRenewCutoff()); len(again) != 0 {
		t.Fatalf("still due after backfill stamped a far-future expiry: %v", again)
	}
}

// TestCertBackfillDueRenews: the same backfill path, but the freshly-observed expiry is already
// within the window - observe, stamp, and promote straight to RenewingCerts.
func TestCertBackfillDueRenews(t *testing.T) {
	r, st := newTestReconciler(t)
	r.CertRenewWindow = 30 * 24 * time.Hour
	cfg := &certCfg{expiry: time.Now().Add(5 * 24 * time.Hour), renewExpiry: time.Now().AddDate(1, 0, 0)}
	r.Cfg = cfg

	readyCluster(t, r, st, "cb2")

	step(t, r, st, "cb2") // observe near expiry -> RenewingCerts
	if got, _ := st.GetCluster("cb2"); got.Phase != domain.PhaseRenewingCerts {
		t.Fatalf("phase = %s, want RenewingCerts", got.Phase)
	}
	if cfg.expiryCalls != 1 {
		t.Fatalf("CertExpiry calls = %d, want 1", cfg.expiryCalls)
	}
	step(t, r, st, "cb2") // renew
	if got, _ := st.GetCluster("cb2"); got.Phase != domain.PhaseReady || cfg.renewCalls != 1 {
		t.Fatalf("after renew: phase=%s renewCalls=%d", got.Phase, cfg.renewCalls)
	}
}

// TestUpgradePrecedesCertRenewal: a pending bundle promotion outranks a due renewal (an upgrade
// reissues certs anyway), so the Ready tick chooses Upgrading and never calls the cert seams.
func TestUpgradePrecedesCertRenewal(t *testing.T) {
	r, st := newTestReconciler(t)
	r.CertRenewWindow = 30 * 24 * time.Hour
	cfg := &certCfg{}
	r.Cfg = cfg

	soon := time.Now().Add(3 * 24 * time.Hour)
	c := &domain.Cluster{
		ID: "up1", Name: "up1", Size: "small", K8sVersion: "1.36.2",
		Phase: domain.PhaseReady, Generation: 1, ObservedGeneration: 1,
		Bundle: "2026.1", TargetBundle: "2026.2", CertNotAfter: &soon,
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	step(t, r, st, "up1")
	if got, _ := st.GetCluster("up1"); got.Phase != domain.PhaseUpgrading {
		t.Fatalf("phase = %s, want Upgrading (upgrade outranks cert renewal)", got.Phase)
	}
	if cfg.expiryCalls != 0 || cfg.renewCalls != 0 {
		t.Fatalf("cert seams called while an upgrade was pending: expiry=%d renew=%d", cfg.expiryCalls, cfg.renewCalls)
	}
}

// TestCertRenewalDisabled: with the window at 0 the feature is off entirely - a Ready cluster with an
// imminent (even nil) expiry is neither surfaced as work nor touched by the cert seams.
func TestCertRenewalDisabled(t *testing.T) {
	r, st := newTestReconciler(t) // CertRenewWindow defaults to 0 (disabled)
	cfg := &certCfg{expiry: time.Now().Add(24 * time.Hour), renewExpiry: time.Now().AddDate(1, 0, 0)}
	r.Cfg = cfg

	c := readyCluster(t, r, st, "cd1")
	soon := time.Now().Add(24 * time.Hour)
	c.CertNotAfter = &soon
	if err := st.UpdateCluster(c); err != nil {
		t.Fatal(err)
	}

	if work, _ := r.clustersNeedingWork(); len(work) != 0 {
		t.Fatalf("disabled: clustersNeedingWork = %v, want none", work)
	}
	ready, _ := st.GetCluster("cd1")
	if err := r.reconcileOne(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	if cfg.expiryCalls != 0 || cfg.renewCalls != 0 {
		t.Fatalf("disabled: cert seams called: expiry=%d renew=%d", cfg.expiryCalls, cfg.renewCalls)
	}
	if got, _ := st.GetCluster("cd1"); got.Phase != domain.PhaseReady {
		t.Fatalf("disabled: phase drifted to %s", got.Phase)
	}
}
