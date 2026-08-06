package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
	"github.com/Daniel-Vaz/KaaS-demo/internal/registry"
)

// registryRecorder records what the reconciler provisions and releases, in order (sharing the
// provisioner's destroy log where wired), so the tests can assert the SEQUENCE - a cluster's project
// must be gone before its infrastructure is. Same shape as vaultRecorder.
type registryRecorder struct {
	events  []string
	destroy *[]string
	// minted counts how many times a credential was actually created, as opposed to an existing one
	// being re-reported.
	minted int
	// existing simulates a robot the registry already holds: EnsureCluster then returns the identity
	// with no secret, which is what the real client does on a re-run.
	existing bool
}

func (g *registryRecorder) EnsurePlatform(context.Context) error { return nil }

// EnsureAuth is never called from the reconcile loop - it runs on the API, which is the only process
// holding the directory settings (see registry.Manager.EnsureAuth). Recorded so that a reconciler
// that started calling it would fail a test rather than silently move where auth is written.
func (g *registryRecorder) EnsureAuth(context.Context) error {
	g.log("ensure-auth")
	return nil
}

func (g *registryRecorder) EnsureCluster(_ context.Context, c *domain.Cluster) (registry.RobotCredential, error) {
	g.log("ensure " + c.ID)
	if g.existing {
		return registry.RobotCredential{Username: "robot$kaas-cluster-" + c.ID}, nil
	}
	g.minted++
	g.existing = true
	return registry.RobotCredential{
		Username: "robot$kaas-cluster-" + c.ID,
		Secret:   "s3cret",
		Expires:  time.Now().Add(365 * 24 * time.Hour),
	}, nil
}

func (g *registryRecorder) ReleaseCluster(_ context.Context, c *domain.Cluster) error {
	g.log("release " + c.ID)
	return nil
}

func (g *registryRecorder) SyncAccess(context.Context, registry.AccessSnapshot) error { return nil }

func (g *registryRecorder) SetUserPassword(context.Context, string, string) error { return nil }

func (g *registryRecorder) log(s string) {
	g.events = append(g.events, s)
	if g.destroy != nil {
		*g.destroy = append(*g.destroy, s)
	}
}

func registryCluster(id, name string) *domain.Cluster {
	return &domain.Cluster{
		ID: id, Name: name, K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
	}
}

// TestRegistryWiredOnce: a cluster gets its project exactly once, and RegistryWired records it so
// later ticks don't re-provision. Note there is no add-on gate here (unlike the Vault wiring) - every
// cluster gets somewhere to push.
func TestRegistryWiredOnce(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &registryRecorder{}
	r.Registry = rec
	if err := st.CreateCluster(registryCluster("g1", "dev")); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "g1")

	if len(rec.events) != 1 || rec.events[0] != "ensure g1" {
		t.Fatalf("registry events = %v, want exactly [ensure g1]", rec.events)
	}
	got, _ := st.GetCluster("g1")
	if !got.RegistryWired {
		t.Fatal("RegistryWired not set - the project would be re-provisioned every tick")
	}
	if got.RegistryRobotNotAfter == nil {
		t.Error("RegistryRobotNotAfter not stamped - a rotation sweep would have no due-date to read")
	}
}

// TestRegistryReleasedBeforeDestroy is the invariant that matters most, and it is sharper here than
// for Vault or DNS: a project is named after the CLUSTER, and cluster names are reusable once a
// cluster is gone - so a project that outlived its cluster would be silently inherited by the next
// cluster of that name, handing one tenant another tenant's images.
func TestRegistryReleasedBeforeDestroy(t *testing.T) {
	r, st := newTestReconciler(t)
	var log []string
	rec := &registryRecorder{destroy: &log}
	r.Registry = rec
	r.Prov = &destroyRecorder{Fake: provision.NewFake(), log: &log}

	if err := st.CreateCluster(registryCluster("g3", "doomed")); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "g3")

	c, _ := st.GetCluster("g3")
	c.Phase = domain.PhaseDeleting
	c.Generation++
	if err := st.UpdateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "g3")

	if len(log) != 3 || log[0] != "ensure g3" || log[1] != "release g3" || log[2] != "destroy g3" {
		t.Fatalf("sequence = %v, want ensure → release → destroy", log)
	}
}

// TestRegistryCredentialSurvivesARetry pins the awkward case: the registry returns a robot's secret
// only when it is first created, so a wiring pass that fails AFTER minting must reuse the sealed copy
// rather than storing a secretless credential - which would hand the cluster a pull Secret that
// cannot authenticate, and only at the moment a pod tried to pull.
func TestRegistryCredentialSurvivesARetry(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &registryRecorder{}
	r.Registry = rec
	c := registryCluster("g4", "retry")
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "g4")

	got, _ := st.GetCluster("g4")
	// Force the wiring to run again against a registry that already holds the robot.
	got.RegistryWired = false
	if err := st.UpdateCluster(got); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileRegistryWiring(context.Background(), got); err != nil {
		t.Fatalf("re-running the wiring against an existing robot failed: %v", err)
	}
	if rec.minted != 1 {
		t.Errorf("credential minted %d times, want 1 - a re-run must not rotate it", rec.minted)
	}
	cred, err := r.storedRegistryCredential(got)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Secret != "s3cret" {
		t.Errorf("stored secret = %q, want the one minted on the first pass", cred.Secret)
	}
}

// TestRegistryAbsentIsHarmless: a deployment with no registry (a nil seam) reconciles normally and
// never marks a cluster wired. Every call site is guarded, which is what lets hand-built test
// reconcilers - and a deployment that wants no registry - leave it nil.
func TestRegistryAbsentIsHarmless(t *testing.T) {
	r, st := newTestReconciler(t)
	r.Registry = nil
	if err := st.CreateCluster(registryCluster("g5", "none")); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "g5")
	got, _ := st.GetCluster("g5")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready", got.Phase)
	}
	if got.RegistryWired {
		t.Error("RegistryWired set with no registry configured")
	}
}
