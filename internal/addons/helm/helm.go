// Package helm is the real addons.Manager: it installs cluster add-ons with
// `helm upgrade --install` (idempotent), using chart/repo/version/values from the catalog
// and the cluster's admin kubeconfig.
//
// The CNI is NOT installed here - it's handled during cluster bootstrap by the Ansible
// config manager. This manager only sees the non-CNI add-ons on the cluster.
package helm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/addons"
	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kubeconfig"
	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
)

type Config struct {
	Bin          string           // "helm"
	KubectlBin   string           // "kubectl" - used to wait for a CRD-providing add-on's CRDs to Establish
	Catalog      *catalog.Catalog // source of chart/repo/values
	WorkDir      string           // per-cluster dir for the temp kubeconfig
	KubeProxyURL string           // SOCKS proxy to reach the API server through; "" (local KVM) = direct
	Events       events.Sink      // optional; streams helm output as events
	Log          *slog.Logger     // required
	// Extras optionally supplies deployment-derived, per-cluster values (and a credential Secret)
	// for an add-on that the catalog cannot express - today external-dns's DNS server, credentials
	// and per-cluster domain filter. See addons.ExtrasFunc.
	Extras addons.ExtrasFunc
}

type Manager struct{ cfg Config }

func New(cfg Config) (*Manager, error) {
	if cfg.Bin == "" {
		cfg.Bin = "helm"
	}
	if cfg.KubectlBin == "" {
		cfg.KubectlBin = "kubectl"
	}
	if cfg.Catalog == nil {
		return nil, fmt.Errorf("helm: Catalog is required")
	}
	if cfg.WorkDir == "" {
		return nil, fmt.Errorf("helm: WorkDir is required")
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("helm: Log is required")
	}
	return &Manager{cfg: cfg}, nil
}

// entryFor resolves the chart definition (chart/repo/namespace/version/values/timeout) for an add-on.
// A built-in catalog add-on is looked up by name in the catalog; a CUSTOM add-on (from a user's
// custom catalog) carries its own definition on the record, so it is self-contained - the reconcile
// loop installs it without resolving any tenant catalog. A custom add-on has no curated --set values
// (its values ride in ValuesOverride) and no EstablishCRDs/Timeout.
func (m *Manager) entryFor(a domain.Addon) (catalog.Addon, error) {
	if a.Custom() {
		return catalog.Addon{
			Name:      a.Name,
			Chart:     a.Chart,
			Repo:      a.Repo,
			Namespace: a.Namespace,
			Version:   a.Version,
		}, nil
	}
	entry, ok := m.cfg.Catalog.Addon(a.Name)
	if !ok {
		return catalog.Addon{}, fmt.Errorf("helm: add-on %q not in catalog", a.Name)
	}
	return entry, nil
}

func (m *Manager) Install(ctx context.Context, c *domain.Cluster, a domain.Addon, kubeconfig []byte) error {
	entry, err := m.entryFor(a)
	if err != nil {
		return err
	}
	kcPath, err := m.writeKubeconfig(c.ID, kubeconfig)
	if err != nil {
		return err
	}
	emit := func(line string) {
		m.cfg.Log.Info("helm", "cluster", c.ID, "addon", a.Name, "line", line)
		if m.cfg.Events != nil {
			m.cfg.Events.Emit(events.Event{ClusterID: c.ID, Level: "info", Source: "addon", Message: line})
		}
	}
	// Clear a release left in a pending state by a previously interrupted Helm run, else the
	// install below fails permanently with "another operation ... is in progress".
	if err := m.recoverIfStuck(ctx, a.Name, namespaceOf(entry, a.Name), kcPath, emit); err != nil {
		return err
	}
	// A per-cluster values override replaces the curated catalog --set (it was seeded from chart
	// defaults + those same overrides, so it is self-contained). Write it to a file for `-f`.
	valuesPath := ""
	if a.ValuesOverride != "" {
		valuesPath, err = m.writeValues(c.ID, a.Name, a.ValuesOverride)
		if err != nil {
			return err
		}
		emit(fmt.Sprintf("using custom values override for %q", a.Name))
	}
	// Deployment-derived configuration the catalog can't carry (external-dns's DNS wiring): the
	// credential Secret is ensured first, since the chart's pod references it, and the values are
	// appended last so they beat both the catalog's and the user's.
	extras := addons.Extras{}
	if m.cfg.Extras != nil {
		extras = m.cfg.Extras(c, a)
	}
	if extras.Secret != nil {
		if err := m.ensureSecret(ctx, *extras.Secret, kcPath, emit); err != nil {
			return err
		}
	}
	args := helmArgs(a.Name, c.ID, entry, a.Version, kcPath, valuesPath)
	args = append(args, setArgs(extras.Values)...)
	if err := procstream.Run(ctx, "", os.Environ(), emit, m.cfg.Bin, args...); err != nil {
		return fmt.Errorf("helm install %q: %w", a.Name, err)
	}
	// If this add-on provides CRDs that later add-ons depend on (kube-prometheus-stack's
	// ServiceMonitor et al.), block until the API server serves them. `helm --wait` above only
	// waited for this release's workloads, not for its CRDs to Establish - so without this the very
	// next add-on's `helm install` renders a ServiceMonitor and fails with "no matches for kind …".
	if len(entry.EstablishCRDs) > 0 {
		if err := m.awaitCRDsEstablished(ctx, entry.EstablishCRDs, kcPath, emit); err != nil {
			return err
		}
	}
	return nil
}

// awaitCRDsEstablished blocks until each named CRD (fully-qualified `plural.group`) reaches its
// `Established` condition, then invalidates the on-disk API discovery cache. Both steps matter:
// the wait defeats the race (a dependent add-on installs only once the kind is actually served),
// and the cache purge defeats the stickiness - the CRD-provider's own helm run populated
// ~/.kube/cache/discovery *before* it created these CRDs, so a later helm process would keep
// rendering against that stale cache (a ~long TTL) and fail to map the new kind. Clearing it forces
// the next add-on to re-discover the live API. Idempotent and safe to re-run on reconcile retries.
func (m *Manager) awaitCRDsEstablished(ctx context.Context, crds []string, kcPath string, emit func(string)) error {
	for _, crd := range crds {
		emit(fmt.Sprintf("waiting for CRD %q to be established", crd))
		args := []string{
			"wait", "--for=condition=Established", "crd/" + crd,
			"--kubeconfig", kcPath, "--timeout=120s",
		}
		if err := procstream.Run(ctx, "", os.Environ(), emit, m.cfg.KubectlBin, args...); err != nil {
			return fmt.Errorf("await CRD %q established: %w", crd, err)
		}
	}
	m.invalidateDiscoveryCache(emit)
	return nil
}

// invalidateDiscoveryCache removes the shared kubectl/helm on-disk API discovery cache so the next
// helm invocation re-discovers the API groups - picking up CRDs installed since the cache was
// written. Best-effort: a failure here only costs a stale cache, so it is logged, not fatal.
func (m *Manager) invalidateDiscoveryCache(emit func(string)) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".kube", "cache", "discovery")
	if err := os.RemoveAll(dir); err != nil {
		emit(fmt.Sprintf("warning: could not clear discovery cache %q: %v", dir, err))
	}
}

// recoverIfStuck detects a release wedged in a pending state - the residue of a Helm process
// that was killed mid-operation (e.g. a reconcile that timed out) - and clears it so the
// subsequent `helm upgrade --install` can proceed. A `pending-install` never had a good
// revision, so it is uninstalled; a `pending-upgrade`/`pending-rollback` is rolled back to the
// last deployed revision. A missing release or an unreachable cluster is a no-op (the install
// then surfaces the real error).
func (m *Manager) recoverIfStuck(ctx context.Context, name, namespace, kcPath string, emit func(string)) error {
	out, err := procstream.Capture(ctx, "", os.Environ(), func(string) {}, m.cfg.Bin,
		"status", name, "--namespace", namespace, "--kubeconfig", kcPath, "-o", "json")
	if err != nil {
		return nil // no release yet, or cluster not reachable - nothing to recover
	}
	var st struct {
		Info struct {
			Status string `json:"status"`
		} `json:"info"`
	}
	if err := json.Unmarshal(out, &st); err != nil || !strings.HasPrefix(st.Info.Status, "pending-") {
		return nil // healthy (deployed/failed/…) - upgrade --install handles it
	}
	emit(fmt.Sprintf("release %q stuck in %q (interrupted operation) - recovering", name, st.Info.Status))

	var recovery []string
	if st.Info.Status == "pending-install" {
		recovery = []string{"uninstall", name, "--namespace", namespace, "--kubeconfig", kcPath, "--wait", "--ignore-not-found"}
	} else {
		recovery = []string{"rollback", name, "--namespace", namespace, "--kubeconfig", kcPath, "--wait"}
	}
	if err := procstream.Run(ctx, "", os.Environ(), emit, m.cfg.Bin, recovery...); err != nil {
		return fmt.Errorf("helm recover %q from %s: %w", name, st.Info.Status, err)
	}
	return nil
}

// Uninstall removes an add-on release (removing an add-on from a live cluster). Idempotent:
// `--ignore-not-found` makes a repeat run a no-op. The namespace is left in place.
func (m *Manager) Uninstall(ctx context.Context, c *domain.Cluster, a domain.Addon, kubeconfig []byte) error {
	kcPath, err := m.writeKubeconfig(c.ID, kubeconfig)
	if err != nil {
		return err
	}
	emit := func(line string) {
		m.cfg.Log.Info("helm", "cluster", c.ID, "addon", a.Name, "line", line)
		if m.cfg.Events != nil {
			m.cfg.Events.Emit(events.Event{ClusterID: c.ID, Level: "info", Source: "addon", Message: line})
		}
	}
	// The catalog entry pins the namespace/timeout; a custom add-on carries its own on the record.
	// Tolerate a missing built-in entry (an add-on removed from the catalog) by falling back to the
	// release-name namespace and default timeout.
	entry, _ := m.entryFor(a)
	args := []string{
		"uninstall", a.Name,
		"--namespace", namespaceOf(entry, a.Name),
		"--kubeconfig", kcPath,
		"--ignore-not-found",
		"--wait", "--timeout", timeoutOf(entry),
	}
	if err := procstream.Run(ctx, "", os.Environ(), emit, m.cfg.Bin, args...); err != nil {
		return fmt.Errorf("helm uninstall %q: %w", a.Name, err)
	}
	return nil
}

// helmArgs builds an idempotent `helm upgrade --install` invocation. The add-on's version is
// the bundle-pinned one (from domain.Addon); chart/repo/values come from the catalog entry.
// A classic HTTP chart repo is passed inline via --repo, so no `helm repo add` state is needed; an
// OCI chart (an oci:// ref in Chart) carries its registry in the ref and takes no --repo. Pure
// function (unit-tested).
//
// valuesFile is a per-cluster full values override: when non-empty it is passed via `-f` and the
// catalog `--set` overrides are skipped (the override was seeded from chart defaults + those same
// overrides, so it is self-contained). When empty, the catalog `--set` overrides apply as before.
//
// clusterID substitutes the `{{.ClusterID}}` token in catalog `--set` values - how kube-prometheus-
// stack's Grafana/Prometheus/Alertmanager get a per-cluster route-prefix so the Monitoring page's
// "Open UI" tunnel can serve their web UIs under /api/clusters/<id>/proxy/<app> (see internal/tunnel).
func helmArgs(name, clusterID string, entry catalog.Addon, version, kubeconfig, valuesFile string) []string {
	args := []string{"upgrade", "--install", name, entry.Chart}
	// An OCI chart reference (oci://…) is self-contained: the registry lives in the ref itself, so
	// it takes no --repo (unlike a classic HTTP chart repo, which is passed inline via --repo).
	if !strings.HasPrefix(entry.Chart, "oci://") {
		args = append(args, "--repo", entry.Repo)
	}
	args = append(args,
		"--version", version,
		"--namespace", namespaceOf(entry, name),
		"--create-namespace",
		"--kubeconfig", kubeconfig,
		"--wait", "--timeout", timeoutOf(entry),
	)
	if valuesFile != "" {
		return append(args, "-f", valuesFile)
	}
	keys := make([]string, 0, len(entry.Values))
	for k := range entry.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic ordering
	for _, k := range keys {
		v := strings.ReplaceAll(entry.Values[k], "{{.ClusterID}}", clusterID)
		args = append(args, "--set", k+"="+v)
	}
	return args
}

// setArgs renders extra values as `--set` arguments, sorted for determinism. They are appended
// after everything else, and helm gives the last `--set` for a key the win - which is the point:
// these carry the platform's own wiring and must survive a user's values override.
func setArgs(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, "--set", k+"="+values[k])
	}
	return args
}

// ensureSecret creates or updates a credential Secret in the add-on's namespace before the chart
// installs, so the chart can reference it instead of carrying the credential in its values (where it
// would land in the Deployment's env, readable by anyone in the cluster who can read a pod spec).
//
// `kubectl apply` of a generated manifest, so it is idempotent like everything else in the loop. The
// manifest is written 0600 and removed straight after: it holds the plaintext credential, and the
// worker's work dir is not a place to leave one lying around. --create-namespace on the install is
// too late for us, so the namespace is ensured here too.
func (m *Manager) ensureSecret(ctx context.Context, spec addons.SecretSpec, kcPath string, emit func(string)) error {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\n", spec.Namespace)
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\ntype: Opaque\nmetadata:\n  name: %s\n  namespace: %s\ndata:\n",
		spec.Name, spec.Namespace)
	keys := make([]string, 0, len(spec.Data))
	for k := range spec.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %s\n", k, base64.StdEncoding.EncodeToString([]byte(spec.Data[k])))
	}
	dir := filepath.Join(m.cfg.WorkDir, "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, spec.Namespace+"-"+spec.Name+".yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	defer os.Remove(path)
	emit(fmt.Sprintf("ensuring secret %s/%s", spec.Namespace, spec.Name))
	if err := procstream.Run(ctx, "", os.Environ(), emit, m.cfg.KubectlBin,
		"apply", "--kubeconfig", kcPath, "-f", path); err != nil {
		return fmt.Errorf("ensure secret %s/%s: %w", spec.Namespace, spec.Name, err)
	}
	return nil
}

// namespaceOf returns the add-on's install namespace, defaulting to its release name - the
// historical one-namespace-per-add-on convention - when the catalog entry doesn't pin one.
func namespaceOf(entry catalog.Addon, name string) string {
	if entry.Namespace != "" {
		return entry.Namespace
	}
	return name
}

// timeoutOf returns the add-on's helm --timeout, defaulting to 5m. A heavy chart (the monitoring
// stack) pins a longer one in the catalog.
func timeoutOf(entry catalog.Addon) string {
	if entry.Timeout != "" {
		return entry.Timeout
	}
	return "5m"
}

func (m *Manager) writeKubeconfig(clusterID string, kc []byte) (string, error) {
	dir := filepath.Join(m.cfg.WorkDir, clusterID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// helm speaks to the API server through client-go, so a remote-KVM proxy-url in the kubeconfig
	// is all it takes to route the release through the tunnel. No-op when the KVM host is local.
	kc, err := kubeconfig.WithProxy(kc, m.cfg.KubeProxyURL)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, kc, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// writeValues persists a per-cluster add-on values override to a file for `helm -f`. One file per
// add-on under the cluster's workdir; overwritten each install so it always reflects the current
// desired override.
func (m *Manager) writeValues(clusterID, addon, override string) (string, error) {
	dir := filepath.Join(m.cfg.WorkDir, clusterID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "values-"+addon+".yaml")
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
