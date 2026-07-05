package helm

import (
	"reflect"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// TestEntryForCustomAddon: a custom add-on (from a user's custom catalog) is self-contained - the
// manager builds its chart definition from the record, not the catalog, so it installs with the
// record's chart/repo/namespace and its values via -f (no catalog --set).
func TestEntryForCustomAddon(t *testing.T) {
	cat, err := catalog.Default()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	m := &Manager{cfg: Config{Catalog: cat}}
	a := domain.Addon{
		Name: "podinfo", Version: "6.5.0", CatalogID: "cat1",
		Chart: "podinfo", Repo: "https://example.com/charts",
		ValuesOverride: "replicaCount: 2\n",
	}
	entry, err := m.entryFor(a)
	if err != nil {
		t.Fatalf("entryFor: %v", err)
	}
	if entry.Chart != "podinfo" || entry.Repo != "https://example.com/charts" || len(entry.Values) != 0 {
		t.Fatalf("custom entry not built from the record: %+v", entry)
	}
	// A custom add-on installs with its values via -f (the caller passes the override file), the
	// record's namespace default (its name), and no catalog --set.
	got := helmArgs(a.Name, "c-1", entry, a.Version, "/tmp/kc", "/tmp/values.yaml")
	if argValue(got, "--namespace") != "podinfo" || argValue(got, "-f") != "/tmp/values.yaml" {
		t.Fatalf("custom helm args wrong: %v", got)
	}
	for _, s := range got {
		if s == "--set" {
			t.Fatalf("custom add-on must have no catalog --set: %v", got)
		}
	}
}

func TestHelmArgs(t *testing.T) {
	entry := catalog.Addon{
		Name:   "metrics-server",
		Chart:  "metrics-server",
		Repo:   "https://kubernetes-sigs.github.io/metrics-server",
		Values: map[string]string{"args[0]": "--kubelet-insecure-tls"},
	}
	got := helmArgs("metrics-server", "c-1", entry, "3.13.1", "/tmp/kc", "")
	want := []string{
		"upgrade", "--install", "metrics-server", "metrics-server",
		"--repo", "https://kubernetes-sigs.github.io/metrics-server",
		"--version", "3.13.1",
		"--namespace", "metrics-server",
		"--create-namespace",
		"--kubeconfig", "/tmp/kc",
		"--wait", "--timeout", "5m",
		"--set", "args[0]=--kubelet-insecure-tls",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("helmArgs mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// TestHelmArgsValuesFile checks a per-cluster values override is passed via -f and suppresses the
// catalog --set flags (the override is self-contained).
func TestHelmArgsValuesFile(t *testing.T) {
	entry := catalog.Addon{
		Name:   "metrics-server",
		Chart:  "metrics-server",
		Repo:   "https://kubernetes-sigs.github.io/metrics-server",
		Values: map[string]string{"args[0]": "--kubelet-insecure-tls"},
	}
	got := helmArgs("metrics-server", "c-1", entry, "3.13.1", "/tmp/kc", "/tmp/values.yaml")
	if f := argValue(got, "-f"); f != "/tmp/values.yaml" {
		t.Fatalf("-f = %q, want /tmp/values.yaml", f)
	}
	for _, a := range got {
		if a == "--set" {
			t.Fatalf("--set must be suppressed when a values file is used: %v", got)
		}
	}
}

// TestHelmArgsNamespaceTimeout checks a catalog entry's pinned namespace and timeout flow into the
// helm invocation (kube-prometheus-stack lands in monitoring-system with a longer timeout).
func TestHelmArgsNamespaceTimeout(t *testing.T) {
	entry := catalog.Addon{
		Name:      "kube-prometheus-stack",
		Chart:     "kube-prometheus-stack",
		Repo:      "https://prometheus-community.github.io/helm-charts",
		Namespace: "monitoring-system",
		Timeout:   "12m",
	}
	got := helmArgs("kube-prometheus-stack", "c-1", entry, "87.12.3", "/tmp/kc", "")
	if ns := argValue(got, "--namespace"); ns != "monitoring-system" {
		t.Fatalf("--namespace = %q, want monitoring-system", ns)
	}
	if to := argValue(got, "--timeout"); to != "12m" {
		t.Fatalf("--timeout = %q, want 12m", to)
	}
}

// TestHelmArgsOCI checks an OCI chart reference (oci://…) is passed as the chart ref with no --repo
// (the registry is carried in the ref), while --version/namespace/timeout still flow through. This
// is the path Envoy Gateway's OCI chart depends on.
func TestHelmArgsOCI(t *testing.T) {
	entry := catalog.Addon{
		Name:      "envoy-gateway",
		Chart:     "oci://docker.io/envoyproxy/gateway-helm",
		Repo:      "",
		Namespace: "envoy-gateway-system",
		Timeout:   "8m",
	}
	got := helmArgs("envoy-gateway", "c-1", entry, "v1.8.2", "/tmp/kc", "")
	for _, a := range got {
		if a == "--repo" {
			t.Fatalf("OCI chart must not pass --repo: %v", got)
		}
	}
	if got[3] != "oci://docker.io/envoyproxy/gateway-helm" {
		t.Fatalf("chart ref = %q, want the oci:// ref", got[3])
	}
	if v := argValue(got, "--version"); v != "v1.8.2" {
		t.Fatalf("--version = %q, want v1.8.2", v)
	}
	if ns := argValue(got, "--namespace"); ns != "envoy-gateway-system" {
		t.Fatalf("--namespace = %q, want envoy-gateway-system", ns)
	}
}

// TestHelmArgsClusterIDTemplating checks the {{.ClusterID}} token in a catalog --set value is
// substituted with the cluster ID - how kube-prometheus-stack's UIs get a per-cluster route-prefix
// for the Monitoring page's "Open UI" tunnel (see internal/tunnel).
func TestHelmArgsClusterIDTemplating(t *testing.T) {
	entry := catalog.Addon{
		Chart: "kube-prometheus-stack",
		Repo:  "https://prometheus-community.github.io/helm-charts",
		Values: map[string]string{
			"prometheus.prometheusSpec.routePrefix": "/api/clusters/{{.ClusterID}}/proxy/prometheus",
		},
	}
	got := helmArgs("kube-prometheus-stack", "abc123", entry, "87.12.3", "/tmp/kc", "")
	if v := argValue(got, "--set"); v != "prometheus.prometheusSpec.routePrefix=/api/clusters/abc123/proxy/prometheus" {
		t.Fatalf("--set = %q, want the cluster id substituted", v)
	}
}

// argValue returns the value following the first occurrence of flag in args ("" if absent).
func argValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// TestHelmArgsNoValues ensures an add-on without values produces no trailing --set.
func TestHelmArgsNoValues(t *testing.T) {
	entry := catalog.Addon{Chart: "foo", Repo: "https://example/repo"}
	got := helmArgs("foo", "c-1", entry, "1.2.3", "/tmp/kc", "")
	for _, a := range got {
		if a == "--set" {
			t.Fatalf("unexpected --set in %v", got)
		}
	}
}
