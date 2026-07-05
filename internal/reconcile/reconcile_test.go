package reconcile

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/addons"
	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/health"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
	"github.com/Daniel-Vaz/KaaS-demo/internal/secrets"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// newTestReconciler builds a reconciler backed entirely by fakes, using the real (embedded)
// catalog - it has a single bundle, so this fits every test that doesn't drive an upgrade.
// pool is the default node pool with n workers at the cluster size these tests all use. Workers now
// live in pools, so a test that wants N of them describes the pool that holds them.
func pool(n int) []domain.NodePool {
	return []domain.NodePool{{Name: domain.DefaultPoolName, Size: "small", DesiredWorkers: n}}
}

func newTestReconciler(t *testing.T) (*Reconciler, store.Store) {
	t.Helper()
	cat, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	return newTestReconcilerWithCatalog(t, cat)
}

// upgradeChainCatalog is a test-local fixture with a multi-hop bundle chain, so the upgrade
// tests can exercise k8s-only, add-on-only, and OS-only hops in isolation - coverage the real
// catalog.json can't provide once it collapses to a single current bundle. Its head ("2026.1")
// mirrors the real catalog's bundle exactly; the earlier hops are synthetic history leading up
// to it.
func upgradeChainCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Parse([]byte(`{
		"os": [
			{"name": "ubuntu-22.04", "family": "ubuntu", "release": "22.04", "status": "supported", "baseImageURL": "", "goldenImage": "ubuntu-22.04-k8s-1.35.6.qcow2"},
			{"name": "ubuntu-24.04", "family": "ubuntu", "release": "24.04", "status": "supported", "baseImageURL": "", "goldenImage": "ubuntu-24.04-k8s-1.36.2.qcow2"}
		],
		"kubernetes": [
			{"version": "1.35.6", "status": "supported"},
			{"version": "1.36.2", "status": "supported"}
		],
		"addons": [
			{"name": "cilium", "type": "cni", "version": "1.19.5", "status": "supported", "repo": "https://helm.cilium.io", "chart": "cilium"},
			{"name": "metrics-server", "type": "addon", "version": "3.13.1", "status": "supported", "repo": "https://kubernetes-sigs.github.io/metrics-server", "chart": "metrics-server"}
		],
		"bundles": [
			{"name": "2025.1", "status": "supported", "os": "ubuntu-22.04", "kubernetes": "1.35.6", "cni": "cilium", "addons": {"cilium": "1.19.5", "metrics-server": "3.12.2"}, "supersedes": ""},
			{"name": "2025.2", "status": "supported", "os": "ubuntu-22.04", "kubernetes": "1.35.6", "cni": "cilium", "addons": {"cilium": "1.19.5", "metrics-server": "3.13.1"}, "supersedes": "2025.1"},
			{"name": "2025.3", "status": "supported", "os": "ubuntu-22.04", "kubernetes": "1.36.2", "cni": "cilium", "addons": {"cilium": "1.19.5", "metrics-server": "3.13.1"}, "supersedes": "2025.2"},
			{"name": "2026.1", "status": "supported", "os": "ubuntu-24.04", "kubernetes": "1.36.2", "cni": "cilium", "addons": {"cilium": "1.19.5", "metrics-server": "3.13.1"}, "supersedes": "2025.3"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// newTestReconcilerWithCatalog builds a reconciler backed entirely by fakes, against the given
// catalog.
func newTestReconcilerWithCatalog(t *testing.T, cat *catalog.Catalog) (*Reconciler, store.Store) {
	t.Helper()
	box, err := secrets.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	r := &Reconciler{
		Store:   st,
		Prov:    provision.NewFake(),
		Cfg:     config.NewFake(),
		Addons:  addons.NewFake(),
		Catalog: cat,
		Secrets: box,
		Events:  events.NewBroker(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return r, st
}

// TestConvergesToReady drives a Pending cluster to Ready by ticking reconcileOne, and
// checks nodes were provisioned and observedGeneration caught up.
func TestConvergesToReady(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{
		ID: "c1", Name: "demo", K8sVersion: "1.36.2", Size: "small",
		NodePools: pool(2), CNI: "cilium", Phase: domain.PhasePending, Generation: 1,
		Addons: []domain.Addon{{Name: "metrics-server", Version: "latest", Phase: "pending"}},
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}

	converge(t, r, st, "c1")

	got, _ := st.GetCluster("c1")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready", got.Phase)
	}
	if got.ObservedGeneration != got.Generation {
		t.Fatalf("observedGeneration %d != generation %d", got.ObservedGeneration, got.Generation)
	}
	if len(got.Nodes) != 3 { // 1 control plane + 2 workers
		t.Fatalf("nodes = %d, want 3", len(got.Nodes))
	}
	for _, a := range got.Addons {
		if a.Phase != "installed" {
			t.Fatalf("addon %s phase = %s, want installed", a.Name, a.Phase)
		}
	}
	// Reaching Ready also mints and stores the read-only viewer kubeconfig (for read-role members).
	if ct, err := st.GetSecret("c1", domain.SecretKubeconfigViewer); err != nil || len(ct) == 0 {
		t.Fatalf("viewer kubeconfig secret after Ready: len=%d err=%v, want stored", len(ct), err)
	}
}

// TestHAControlPlaneNodes drives an HA cluster (3 control planes) to Ready and checks it
// provisions three control-plane VMs plus its workers.
func TestHAControlPlaneNodes(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{
		ID: "h1", Name: "ha", K8sVersion: "1.36.2", Size: "small",
		ControlPlanes: 3, APIVIP: "192.168.122.240", NodePools: pool(2),
		CNI: "cilium", Phase: domain.PhasePending, Generation: 1,
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "h1")

	got, _ := st.GetCluster("h1")
	if got.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready", got.Phase)
	}
	cps := 0
	for _, n := range got.Nodes {
		if n.Role == domain.RoleControlPlane {
			cps++
		}
	}
	if cps != 3 {
		t.Fatalf("control-plane nodes = %d, want 3", cps)
	}
	if len(got.Nodes) != 5 { // 3 control planes + 2 workers
		t.Fatalf("nodes = %d, want 5", len(got.Nodes))
	}
}

// TestIdempotentReconcile ensures a converged, unchanged cluster is left alone.
func TestIdempotentReconcile(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{ID: "c2", Name: "x", K8sVersion: "1.36.2", Size: "small", Phase: domain.PhasePending, Generation: 1}
	_ = st.CreateCluster(c)
	converge(t, r, st, "c2")

	ready, _ := st.GetCluster("c2")
	if ready.NeedsWork() {
		t.Fatal("ready cluster should not need work")
	}
	// Another reconcile must be a no-op (still Ready).
	if err := r.reconcileOne(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetCluster("c2")
	if after.Phase != domain.PhaseReady {
		t.Fatalf("phase drifted to %s", after.Phase)
	}
}

// TestScaleWorkersDown converges a 3-worker cluster, then scales it to 1 (a generation
// bump) and checks the reconciler drains+drops the extra workers via the Updating path.
func TestScaleWorkersDown(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{
		ID: "s1", Name: "demo", K8sVersion: "1.36.2", Size: "small",
		NodePools: pool(3), CNI: "cilium", Phase: domain.PhasePending, Generation: 1,
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "s1")
	if got, _ := st.GetCluster("s1"); len(got.Nodes) != 4 { // 1 cp + 3 workers
		t.Fatalf("after create: nodes = %d, want 4", len(got.Nodes))
	}

	got, _ := st.GetCluster("s1")
	got.NodePools = pool(1)
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "s1")

	after, _ := st.GetCluster("s1")
	if after.Phase != domain.PhaseReady || after.ObservedGeneration != after.Generation {
		t.Fatalf("phase=%s obsGen=%d gen=%d", after.Phase, after.ObservedGeneration, after.Generation)
	}
	workers := 0
	for _, n := range after.Nodes {
		if n.Role == domain.RoleWorker {
			workers++
		}
	}
	if len(after.Nodes) != 2 || workers != 1 {
		t.Fatalf("after scale-down: nodes=%d workers=%d, want 2 nodes / 1 worker", len(after.Nodes), workers)
	}
}

// TestAddonRemovalReconcile checks reconcileAddons uninstalls a "removing" add-on (dropping
// it from the list), installs a new "pending" one, and leaves "installed" ones untouched.
func TestAddonRemovalReconcile(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{
		ID: "a1", Name: "demo", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
		Addons: []domain.Addon{{Name: "metrics-server", Version: "1", Phase: "pending"}},
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "a1") // installs metrics-server, saves the kubeconfig secret

	got, _ := st.GetCluster("a1")
	got.Addons = []domain.Addon{
		{Name: "metrics-server", Version: "1", Phase: "removing"},
		{Name: "ingress-nginx", Version: "3", Phase: "pending"},
	}
	if err := r.reconcileAddons(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Addons) != 1 {
		t.Fatalf("addons = %d, want 1 (metrics-server removed)", len(got.Addons))
	}
	if got.Addons[0].Name != "ingress-nginx" || got.Addons[0].Phase != "installed" {
		t.Fatalf("surviving add-on = %+v, want ingress-nginx/installed", got.Addons[0])
	}
}

// TestKubernetesUpgradeInPlace promotes a SINGLE-NODE cluster on bundle 2025.1 (k8s 1.35.6) to
// 2025.3 (k8s 1.36.2) - the user's core scenario - and checks it reaches 1.36.2 in place without an
// OS change (the intervening add-on hop is applied too). Single-node k8s upgrades must work.
func TestKubernetesUpgradeInPlace(t *testing.T) {
	r, st := newTestReconcilerWithCatalog(t, upgradeChainCatalog(t))
	c := &domain.Cluster{
		ID: "u1", Name: "up", Size: "small", Phase: domain.PhasePending, Generation: 1,
		Bundle: "2025.1", OSImage: "ubuntu-22.04", K8sVersion: "1.35.6", CNI: "cilium", CNIVersion: "1.19.5",
		Addons: []domain.Addon{{Name: "metrics-server", Version: "3.12.2", Phase: "pending"}},
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "u1")

	// Promote to 2025.3 (k8s 1.36.2, same OS): set the desired target + bump generation, as the API does.
	got, _ := st.GetCluster("u1")
	got.TargetBundle = "2025.3"
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "u1")

	after, _ := st.GetCluster("u1")
	if after.Phase != domain.PhaseReady {
		t.Fatalf("phase = %s, want Ready", after.Phase)
	}
	if after.Bundle != "2025.3" || after.K8sVersion != "1.36.2" {
		t.Fatalf("provenance = %s/%s, want 2025.3/1.36.2", after.Bundle, after.K8sVersion)
	}
	if after.OSImage != "ubuntu-22.04" {
		t.Fatalf("os_image = %q, want ubuntu-22.04 (unchanged by a k8s upgrade)", after.OSImage)
	}
	if len(after.Addons) != 1 || after.Addons[0].Version != "3.13.1" {
		t.Fatalf("metrics-server = %+v, want 3.13.1 (the intervening add-on hop applied)", after.Addons)
	}
	if after.TargetBundle != "" {
		t.Fatalf("target_bundle = %q, want cleared after reaching the target", after.TargetBundle)
	}
	if after.ObservedGeneration != after.Generation {
		t.Fatalf("observedGeneration %d != generation %d", after.ObservedGeneration, after.Generation)
	}
}

// TestAddonHelmUpgrade promotes a single-node cluster on bundle 2025.1 (metrics-server 3.12.2) to
// 2025.2 (metrics-server 3.13.1) - an add-on-only hop - and checks the add-on version is bumped and
// re-installed while OS/Kubernetes are untouched.
func TestAddonHelmUpgrade(t *testing.T) {
	r, st := newTestReconcilerWithCatalog(t, upgradeChainCatalog(t))
	c := &domain.Cluster{
		ID: "u3", Name: "addonup", Size: "small", Phase: domain.PhasePending, Generation: 1,
		Bundle: "2025.1", OSImage: "ubuntu-22.04", K8sVersion: "1.35.6", CNI: "cilium", CNIVersion: "1.19.5",
		Addons: []domain.Addon{{Name: "metrics-server", Version: "3.12.2", Phase: "pending"}},
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "u3")

	got, _ := st.GetCluster("u3")
	got.TargetBundle = "2025.2"
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "u3")

	after, _ := st.GetCluster("u3")
	if after.Phase != domain.PhaseReady || after.Bundle != "2025.2" {
		t.Fatalf("after add-on upgrade: phase=%s bundle=%s, want Ready/2025.2", after.Phase, after.Bundle)
	}
	if after.OSImage != "ubuntu-22.04" || after.K8sVersion != "1.35.6" {
		t.Fatalf("os/k8s = %s/%s, want ubuntu-22.04/1.35.6 (unchanged by an add-on upgrade)", after.OSImage, after.K8sVersion)
	}
	if len(after.Addons) != 1 || after.Addons[0].Version != "3.13.1" || after.Addons[0].Phase != "installed" {
		t.Fatalf("metrics-server = %+v, want version 3.13.1 / installed", after.Addons)
	}
	if after.ObservedGeneration != after.Generation {
		t.Fatalf("observedGeneration %d != generation %d", after.ObservedGeneration, after.Generation)
	}
}

// TestOSRollingReplacement promotes an HA cluster on bundle 2025.3 (ubuntu-22.04) to 2026.1
// (ubuntu-24.04) - an OS-only hop - and checks every node is rolled onto the new golden image.
func TestOSRollingReplacement(t *testing.T) {
	r, st := newTestReconcilerWithCatalog(t, upgradeChainCatalog(t))
	c := &domain.Cluster{
		ID: "u2", Name: "osup", Size: "small", Phase: domain.PhasePending, Generation: 1,
		ControlPlanes: 3, APIVIP: "192.168.122.240", NodePools: pool(2),
		Bundle: "2025.3", OSImage: "ubuntu-22.04", K8sVersion: "1.36.2", CNI: "cilium", CNIVersion: "1.19.5",
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "u2")

	// Every node starts on the ubuntu-22.04 image.
	start, _ := st.GetCluster("u2")
	oldImage := catalog.GoldenImageName("ubuntu-22.04", "1.36.2")
	for _, n := range start.Nodes {
		if n.Image != oldImage {
			t.Fatalf("node %s image = %q, want %q at create", n.VMName, n.Image, oldImage)
		}
	}

	start.TargetBundle = "2026.1"
	start.Generation++
	_ = st.UpdateCluster(start)
	converge(t, r, st, "u2")

	after, _ := st.GetCluster("u2")
	if after.Phase != domain.PhaseReady || after.Bundle != "2026.1" || after.OSImage != "ubuntu-24.04" {
		t.Fatalf("after OS upgrade: phase=%s bundle=%s os=%s", after.Phase, after.Bundle, after.OSImage)
	}
	newImage := catalog.GoldenImageName("ubuntu-24.04", "1.36.2")
	if len(after.Nodes) != 5 {
		t.Fatalf("nodes = %d, want 5 (3 cp + 2 workers)", len(after.Nodes))
	}
	for _, n := range after.Nodes {
		if n.Image != newImage {
			t.Fatalf("node %s image = %q, want %q after rolling replacement", n.VMName, n.Image, newImage)
		}
	}
	if after.ObservedGeneration != after.Generation {
		t.Fatalf("observedGeneration %d != generation %d", after.ObservedGeneration, after.Generation)
	}
}

// TestSingleNodeOSUpgrade promotes a SINGLE-NODE cluster on bundle 2025.3 (ubuntu-22.04) to 2026.1
// (ubuntu-24.04) - an OS hop on the sole control plane - and checks the node is rebuilt onto the new
// image via the backup/restore path (fake config makes Backup/RestoreControlPlane no-ops).
func TestSingleNodeOSUpgrade(t *testing.T) {
	r, st := newTestReconcilerWithCatalog(t, upgradeChainCatalog(t))
	c := &domain.Cluster{
		ID: "u4", Name: "snos", Size: "small", Phase: domain.PhasePending, Generation: 1,
		Bundle: "2025.3", OSImage: "ubuntu-22.04", K8sVersion: "1.36.2", CNI: "cilium", CNIVersion: "1.19.5",
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "u4")

	got, _ := st.GetCluster("u4")
	if len(got.Nodes) != 1 || got.Nodes[0].Role != domain.RoleControlPlane {
		t.Fatalf("expected a single control-plane node, got %+v", got.Nodes)
	}
	got.TargetBundle = "2026.1"
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "u4")

	after, _ := st.GetCluster("u4")
	newImage := catalog.GoldenImageName("ubuntu-24.04", "1.36.2")
	if after.Phase != domain.PhaseReady || after.Bundle != "2026.1" || after.OSImage != "ubuntu-24.04" {
		t.Fatalf("after single-node OS upgrade: phase=%s bundle=%s os=%s", after.Phase, after.Bundle, after.OSImage)
	}
	if len(after.Nodes) != 1 || after.Nodes[0].Image != newImage {
		t.Fatalf("control plane image = %+v, want %q", after.Nodes, newImage)
	}
	if after.ObservedGeneration != after.Generation {
		t.Fatalf("observedGeneration %d != generation %d", after.ObservedGeneration, after.Generation)
	}
}

// missingImageProv wraps the fake provisioner and reports every image as unavailable, standing in
// for a real setup where the target golden image hasn't been baked.
type missingImageProv struct {
	*provision.Fake
}

func (missingImageProv) ImageAvailable(name string) error {
	return fmt.Errorf("golden image %q not found", name)
}

// recordingCfg embeds the fake config manager and records the node names that were drained/removed,
// so a test can assert the reconciler did NOT touch a node it couldn't rebuild.
type recordingCfg struct {
	*config.Fake
	removed []string
}

func (c *recordingCfg) RemoveWorkers(_ context.Context, _ *domain.Cluster, workers []domain.Node) error {
	for _, w := range workers {
		c.removed = append(c.removed, w.VMName)
	}
	return nil
}

func (c *recordingCfg) RemoveControlPlane(_ context.Context, _ *domain.Cluster, n domain.Node) error {
	c.removed = append(c.removed, n.VMName)
	return nil
}

func (c *recordingCfg) BackupControlPlane(_ context.Context, _ *domain.Cluster, n domain.Node) error {
	c.removed = append(c.removed, n.VMName)
	return nil
}

// TestOSUpgradeAbortsWhenImageMissing checks the preflight: if the target golden image isn't
// available, a rolling OS replacement must fail loudly WITHOUT draining/removing any node (which
// would otherwise corrupt the cluster while the store wrongly recorded the new image).
func TestOSUpgradeAbortsWhenImageMissing(t *testing.T) {
	r, st := newTestReconcilerWithCatalog(t, upgradeChainCatalog(t))
	r.Prov = missingImageProv{provision.NewFake()}
	cfg := &recordingCfg{Fake: config.NewFake()}
	r.Cfg = cfg

	c := &domain.Cluster{
		ID: "u5", Name: "noimg", Size: "small", Phase: domain.PhasePending, Generation: 1,
		NodePools: pool(1),
		Bundle:    "2025.3", OSImage: "ubuntu-22.04", K8sVersion: "1.36.2", CNI: "cilium", CNIVersion: "1.19.5",
	}
	_ = st.CreateCluster(c)
	converge(t, r, st, "u5")

	got, _ := st.GetCluster("u5")
	got.TargetBundle = "2026.1" // OS hop ubuntu-22.04 → ubuntu-24.04
	got.Generation++
	_ = st.UpdateCluster(got)

	// Drive the Ready→Upgrading transition, then the upgrade step, which must error.
	got, _ = st.GetCluster("u5")
	if err := r.reconcileOne(context.Background(), got); err != nil { // Ready → Upgrading
		t.Fatal(err)
	}
	got, _ = st.GetCluster("u5")
	if err := r.reconcileOne(context.Background(), got); err == nil {
		t.Fatal("expected the OS upgrade to fail when the golden image is missing")
	}

	if len(cfg.removed) != 0 {
		t.Fatalf("nodes were drained/removed despite the missing image: %v", cfg.removed)
	}
	after, _ := st.GetCluster("u5")
	oldImage := catalog.GoldenImageName("ubuntu-22.04", "1.36.2")
	for _, n := range after.Nodes {
		if n.Image != oldImage {
			t.Fatalf("node %s image = %q, want unchanged %q", n.VMName, n.Image, oldImage)
		}
	}
	if after.OSImage != "ubuntu-22.04" || after.Bundle != "2025.3" {
		t.Fatalf("provenance advanced despite failed upgrade: bundle=%s os=%s", after.Bundle, after.OSImage)
	}
}

// TestOrphanGC checks the GC sweep destroys infrastructure whose cluster is gone (here,
// fully Deleted in the store), leaving zero orphans.
func TestOrphanGC(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{ID: "g1", Name: "ghost", K8sVersion: "1.36.2", Size: "small", Phase: domain.PhasePending, Generation: 1}
	_ = st.CreateCluster(c)
	converge(t, r, st, "g1")

	if m, _ := r.Prov.ListManaged(context.Background()); len(m) != 1 || m[0] != "g1" {
		t.Fatalf("managed infra = %v, want [g1]", m)
	}
	// Simulate the cluster having been fully deleted while its infra lingered (e.g. a crash
	// between marking Deleted and destroying VMs).
	ghost, _ := st.GetCluster("g1")
	ghost.Phase = domain.PhaseDeleted
	_ = st.UpdateCluster(ghost)

	r.GC(context.Background())

	if m, _ := r.Prov.ListManaged(context.Background()); len(m) != 0 {
		t.Fatalf("after GC, managed infra = %v, want none", m)
	}
}

// TestDeleteDuringBringUpWins reproduces the clobber bug: a delete requested while a reconcile
// step is in flight must not be overwritten by that step's stale write, so the cluster tears down
// instead of running its installation to completion. It simulates the race at reconcileOne
// granularity - a worker loads its copy, a delete lands (Phase=Deleting, generation bumped, exactly
// as App.DeleteCluster does), then the worker finishes its phase and persists - and asserts the
// delete survives and the cluster converges to Deleted.
func TestDeleteDuringBringUpWins(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{
		ID: "c1", Name: "demo", K8sVersion: "1.36.2", Size: "small",
		NodePools: pool(1), CNI: "cilium", Phase: domain.PhasePending, Generation: 1,
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}

	// Advance a few phases so the cluster is mid-bring-up (not yet Ready).
	for i := 0; i < 3; i++ {
		cur, _ := st.GetCluster("c1")
		if err := r.reconcileOne(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
	}
	// The in-flight reconcile worker loads its working copy here.
	stale, _ := st.GetCluster("c1")
	if stale.Phase == domain.PhaseReady {
		t.Fatalf("cluster reached Ready too early; test needs it mid-bring-up")
	}

	// A delete lands while that worker is "running" - exactly what App.DeleteCluster writes.
	del, _ := st.GetCluster("c1")
	del.Phase = domain.PhaseDeleting
	del.Generation++
	if err := st.UpdateCluster(del); err != nil {
		t.Fatal(err)
	}

	// The worker finishes its phase and persists its (now stale) copy. It must abandon the write,
	// not resurrect the cluster - and that abandonment is not an error.
	if err := r.reconcileOne(context.Background(), stale); err != nil {
		t.Fatalf("stale reconcile returned error: %v", err)
	}
	got, _ := st.GetCluster("c1")
	if got.Phase != domain.PhaseDeleting {
		t.Fatalf("delete was clobbered: phase = %s, want Deleting", got.Phase)
	}

	// And the cluster tears all the way down.
	converge(t, r, st, "c1")
	got, _ = st.GetCluster("c1")
	if got.Phase != domain.PhaseDeleted {
		t.Fatalf("phase = %s, want Deleted", got.Phase)
	}
	if m, _ := r.Prov.ListManaged(context.Background()); len(m) != 0 {
		t.Fatalf("infra not destroyed after delete: managed = %v", m)
	}
}

// TestCompletesOperationsOnConverge checks the reconciler closes out an in-progress action-history
// operation once the cluster converges to the generation that produced it.
func TestCompletesOperationsOnConverge(t *testing.T) {
	r, st := newTestReconciler(t)
	c := &domain.Cluster{
		ID: "c1", Name: "demo", K8sVersion: "1.36.2", Size: "small",
		NodePools: pool(1), CNI: "cilium", Phase: domain.PhasePending, Generation: 1,
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordOperation(&domain.Operation{
		ID: "op1", ClusterID: "c1", Kind: domain.OpCreate, Summary: "created",
		Generation: 1, Status: domain.OpInProgress, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	converge(t, r, st, "c1")

	ops, err := st.ListOperations("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("operations = %d, want 1", len(ops))
	}
	if ops[0].Status != domain.OpCompleted {
		t.Fatalf("operation status = %s, want completed", ops[0].Status)
	}
	if ops[0].FinishedAt == nil {
		t.Fatal("completed operation has no finished_at")
	}
}

// converge ticks reconcileOne until the cluster stops needing work (bounded).
// TestCheckHealthSavesSnapshot drives a cluster to Ready, then runs the health sweep and checks it
// evaluated and persisted a snapshot (the fake checker reports all-healthy) with one check per
// dedicated check and per-node detail - the full CheckHealth → SaveHealth → GetHealth path.
func TestCheckHealthSavesSnapshot(t *testing.T) {
	r, st := newTestReconciler(t)
	r.Health = health.NewFake()
	c := &domain.Cluster{
		ID: "h1", Name: "healthy", K8sVersion: "1.36.2", Size: "small",
		NodePools: pool(1), CNI: "cilium", Phase: domain.PhasePending, Generation: 1,
		Addons: []domain.Addon{{Name: "metrics-server", Version: "latest", Phase: "pending"}},
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "h1")

	r.CheckHealth(context.Background())

	snap, err := st.GetHealth("h1")
	if err != nil {
		t.Fatalf("GetHealth: %v", err)
	}
	if snap.Status != domain.HealthHealthy {
		t.Errorf("rollup = %s, want healthy", snap.Status)
	}
	// Eight from the Checker itself, plus the two the reconciler appends from stored control-plane
	// state (backup staleness and automatic-repair status) - those need policies the Checker seam
	// deliberately does not carry, so they are added above it. See checkHealthOne.
	if len(snap.Checks) != 10 {
		t.Errorf("got %d checks, want 10", len(snap.Checks))
	}
	if len(snap.Nodes) != 2 { // 1 control plane + 1 worker
		t.Errorf("got %d node-health entries, want 2", len(snap.Nodes))
	}
}

// TestCheckHealthSkipsNonReady ensures the health sweep only touches Ready clusters - a still
// provisioning cluster gets no snapshot (matching the ticker's gating).
func TestCheckHealthSkipsNonReady(t *testing.T) {
	r, st := newTestReconciler(t)
	r.Health = health.NewFake()
	c := &domain.Cluster{ID: "h2", Name: "pending", Size: "small", Phase: domain.PhasePending, Generation: 1}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}

	r.CheckHealth(context.Background())

	if _, err := st.GetHealth("h2"); err != store.ErrNotFound {
		t.Errorf("GetHealth on non-Ready cluster = %v, want ErrNotFound", err)
	}
}

// monitoringWiringRecorder wraps the fake config manager to record EnsureCNIMetrics and
// EnsureControlPlaneMetrics calls and whether the monitoring stack was already installed when each
// happened (the required ordering - both must run only once the stack is up).
type monitoringWiringRecorder struct {
	*config.Fake
	cniCalls, controlPlaneCalls                     int
	cniStackInstalledAtCall, cpStackInstalledAtCall bool
}

func (r *monitoringWiringRecorder) EnsureCNIMetrics(_ context.Context, c *domain.Cluster) error {
	r.cniCalls++
	if monitoringInstalled(c) {
		r.cniStackInstalledAtCall = true
	}
	return nil
}

func (r *monitoringWiringRecorder) EnsureControlPlaneMetrics(_ context.Context, c *domain.Cluster) error {
	r.controlPlaneCalls++
	if monitoringInstalled(c) {
		r.cpStackInstalledAtCall = true
	}
	return nil
}

// gatewayWiringRecorder records EnsureDefaultGateway calls (and the address it was handed) so the
// default MetalLB pool / Envoy Gateway wiring can be asserted.
type gatewayWiringRecorder struct {
	*config.Fake
	calls  int
	lastIP string
}

func (r *gatewayWiringRecorder) EnsureDefaultGateway(_ context.Context, c *domain.Cluster) error {
	r.calls++
	r.lastIP = c.LoadBalancerIP
	return nil
}

// TestGatewayWiringAfterAddons: a cluster with metallb + envoy-gateway and a reserved LoadBalancer IP
// gets the default MetalLB pool / Envoy Gateway applied once, and the GatewayWired marker is set.
func TestGatewayWiringAfterAddons(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &gatewayWiringRecorder{Fake: config.NewFake()}
	r.Cfg = rec
	c := &domain.Cluster{
		ID: "g1", Name: "gw", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1, LoadBalancerIP: "10.200.3.249",
		Addons: []domain.Addon{
			{Name: "metallb", Version: "0.16.1", Phase: "pending"},
			{Name: "envoy-gateway", Version: "v1.8.2", Phase: "pending"},
		},
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "g1")
	if rec.calls != 1 {
		t.Fatalf("EnsureDefaultGateway called %d times, want exactly 1", rec.calls)
	}
	if rec.lastIP != "10.200.3.249" {
		t.Fatalf("EnsureDefaultGateway got IP %q, want the cluster's reserved 10.200.3.249", rec.lastIP)
	}
	got, err := st.GetCluster("g1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.GatewayWired {
		t.Fatal("GatewayWired not set after wiring - it would re-run every tick")
	}
}

// TestGatewayWiringSkippedWithoutBothAddons: only metallb (no envoy-gateway) → the gateway is not
// wired (nor if the reserved IP is missing).
func TestGatewayWiringSkippedWithoutBothAddons(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &gatewayWiringRecorder{Fake: config.NewFake()}
	r.Cfg = rec
	c := &domain.Cluster{
		ID: "g2", Name: "gw2", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1, LoadBalancerIP: "10.200.3.249",
		Addons: []domain.Addon{{Name: "metallb", Version: "0.16.1", Phase: "pending"}},
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "g2")
	if rec.calls != 0 {
		t.Fatalf("EnsureDefaultGateway called %d times without envoy-gateway, want 0", rec.calls)
	}
}

// TestGatewayWiringReadyGatesCertManager: when cert-manager is on the cluster (it terminates HTTPS on
// the default Gateway in the SAME latching wiring pass), the wiring must hold until it is installed -
// otherwise GatewayWired latches with the TLS half never applied.
func TestGatewayWiringReadyGatesCertManager(t *testing.T) {
	base := []domain.Addon{
		{Name: "metallb", Phase: "installed"},
		{Name: "envoy-gateway", Phase: "installed"},
	}
	// metallb + envoy installed, no cert-manager selected → ready.
	if !gatewayWiringReady(&domain.Cluster{LoadBalancerIP: "10.0.0.9", Addons: base}) {
		t.Fatal("want ready with metallb + envoy installed and no cert-manager")
	}
	// cert-manager selected but still installing → NOT ready (hold the whole pass).
	pending := append(append([]domain.Addon{}, base...), domain.Addon{Name: "cert-manager", Phase: "pending"})
	if gatewayWiringReady(&domain.Cluster{LoadBalancerIP: "10.0.0.9", Addons: pending}) {
		t.Fatal("want NOT ready while cert-manager is still installing")
	}
	// cert-manager installed → ready again.
	done := append(append([]domain.Addon{}, base...), domain.Addon{Name: "cert-manager", Phase: "installed"})
	if !gatewayWiringReady(&domain.Cluster{LoadBalancerIP: "10.0.0.9", Addons: done}) {
		t.Fatal("want ready once cert-manager is installed")
	}
	// cert-manager being removed does not hold the wiring back.
	removing := append(append([]domain.Addon{}, base...), domain.Addon{Name: "cert-manager", Phase: "removing"})
	if !gatewayWiringReady(&domain.Cluster{LoadBalancerIP: "10.0.0.9", Addons: removing}) {
		t.Fatal("want ready when cert-manager is being removed")
	}
}

// TestMonitoringWiringAfterStack: a cluster with the kube-prometheus-stack add-on gets both the CNI's
// ServiceMonitors (EnsureCNIMetrics) and the control plane's scrape config (EnsureControlPlaneMetrics)
// wired once the stack is installed - never before.
func TestMonitoringWiringAfterStack(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &monitoringWiringRecorder{Fake: config.NewFake()}
	r.Cfg = rec
	c := &domain.Cluster{
		ID: "c1", Name: "demo", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
		Addons: []domain.Addon{
			{Name: "kube-prometheus-stack", Version: "87.12.3", Phase: "pending"},
			{Name: "metrics-server", Version: "3.13.1", Phase: "pending"},
		},
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "c1")
	if rec.cniCalls == 0 {
		t.Fatal("EnsureCNIMetrics never called though the monitoring stack is installed")
	}
	if !rec.cniStackInstalledAtCall {
		t.Fatal("EnsureCNIMetrics ran before the monitoring stack was installed")
	}
	if rec.controlPlaneCalls == 0 {
		t.Fatal("EnsureControlPlaneMetrics never called though the monitoring stack is installed")
	}
	if !rec.cpStackInstalledAtCall {
		t.Fatal("EnsureControlPlaneMetrics ran before the monitoring stack was installed")
	}
}

// TestMonitoringWiringSkippedWithoutStack: no monitoring stack → neither the CNI nor the control
// plane is re-wired.
func TestMonitoringWiringSkippedWithoutStack(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &monitoringWiringRecorder{Fake: config.NewFake()}
	r.Cfg = rec
	c := &domain.Cluster{
		ID: "c2", Name: "demo2", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
		Addons: []domain.Addon{{Name: "metrics-server", Version: "3.13.1", Phase: "pending"}},
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "c2")
	if rec.cniCalls != 0 {
		t.Fatalf("EnsureCNIMetrics called %d times with no monitoring stack, want 0", rec.cniCalls)
	}
	if rec.controlPlaneCalls != 0 {
		t.Fatalf("EnsureControlPlaneMetrics called %d times with no monitoring stack, want 0", rec.controlPlaneCalls)
	}
}

// TestMonitoringWiringNotRerunOnUnrelatedUpdate: once a cluster is wired, an unrelated update tick
// (here an add-on values edit) must NOT re-run the control-plane / CNI wiring - it's idempotent but
// not free, and re-running it on every update is the noise this marker eliminates.
func TestMonitoringWiringNotRerunOnUnrelatedUpdate(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &monitoringWiringRecorder{Fake: config.NewFake()}
	r.Cfg = rec
	c := &domain.Cluster{
		ID: "c3", Name: "demo3", K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1,
		Addons: []domain.Addon{
			{Name: "kube-prometheus-stack", Version: "87.12.3", Phase: "pending"},
			{Name: "metrics-server", Version: "3.13.1", Phase: "pending"},
		},
	}
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "c3")
	if rec.cniCalls != 1 || rec.controlPlaneCalls != 1 {
		t.Fatalf("after bring-up: cni=%d cp=%d, want 1/1", rec.cniCalls, rec.controlPlaneCalls)
	}
	if got, _ := st.GetCluster("c3"); !got.MonitoringWired {
		t.Fatal("MonitoringWired not set after wiring")
	}

	// Unrelated edit: change an add-on's values, which flips it to "updating" and bumps generation.
	got, _ := st.GetCluster("c3")
	for i := range got.Addons {
		if got.Addons[i].Name == "metrics-server" {
			got.Addons[i].Phase = "updating"
			got.Addons[i].ValuesOverride = "replicas: 2"
		}
	}
	got.Generation++
	_ = st.UpdateCluster(got)
	converge(t, r, st, "c3")

	if rec.cniCalls != 1 || rec.controlPlaneCalls != 1 {
		t.Fatalf("after unrelated update: cni=%d cp=%d, want 1/1 (wiring should be skipped)", rec.cniCalls, rec.controlPlaneCalls)
	}
}

func converge(t *testing.T, r *Reconciler, st store.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := st.GetCluster(id)
		if err != nil {
			t.Fatal(err)
		}
		if !c.NeedsWork() {
			return
		}
		if err := r.reconcileOne(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not converge; stuck at %s", c.Phase)
		}
	}
}
