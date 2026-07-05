package values

import (
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"gopkg.in/yaml.v3"
)

// TestMergedExpandsDottedAndIndexedKeys checks the Helm --set path expander handles nested map keys,
// list indices, and scalar type coercion (bool/int/string), overlaying them onto chart defaults.
func TestMergedExpandsDottedAndIndexedKeys(t *testing.T) {
	entry := catalog.Addon{
		Name:  "metrics-server",
		Chart: "metrics-server",
		Values: map[string]string{
			"args[0]":                             "--kubelet-insecure-tls",
			"metrics.enabled":                     "true",
			"replicas":                            "2",
			"serviceMonitor.enabled":              "false",
			"resources.requests.cpu":              "100m",
			"prometheus.prometheusSpec.retention": "6h",
		},
	}
	// Seed a chart default the override should preserve (image.tag) and one it should replace.
	chart := "image:\n  tag: v0.7.0\nreplicas: 1\n"

	out, err := Merged(entry, chart)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}

	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not valid YAML: %v\n%s", err, out)
	}

	// list index
	args, ok := got["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "--kubelet-insecure-tls" {
		t.Fatalf("args not expanded: %#v", got["args"])
	}
	// bool coercion
	metrics, _ := got["metrics"].(map[string]any)
	if metrics["enabled"] != true {
		t.Fatalf("metrics.enabled not a bool true: %#v", metrics["enabled"])
	}
	// int coercion + override of a chart default
	if got["replicas"] != 2 {
		t.Fatalf("replicas override/int coercion failed: %#v", got["replicas"])
	}
	// nested map created several levels deep
	prom, _ := got["prometheus"].(map[string]any)
	spec, _ := prom["prometheusSpec"].(map[string]any)
	if spec["retention"] != "6h" {
		t.Fatalf("deep nested key failed: %#v", got["prometheus"])
	}
	// string preserved from chart defaults
	img, _ := got["image"].(map[string]any)
	if img["tag"] != "v0.7.0" {
		t.Fatalf("chart default not preserved: %#v", got["image"])
	}
}

// TestMergedIndentAndComments checks the merged output uses Helm's 2-space indent and preserves the
// chart's own comments (so the editor shows the full annotated values.yaml, not a stripped dump).
func TestMergedIndentAndComments(t *testing.T) {
	entry := catalog.Addon{Name: "x", Values: map[string]string{"metrics.enabled": "true"}}
	chart := "# top-level comment\nimage:\n  # tag comment\n  tag: v1\n"
	out, err := Merged(entry, chart)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if strings.Contains(out, "    ") {
		t.Fatalf("expected 2-space indent, found 4 spaces:\n%s", out)
	}
	if !strings.Contains(out, "# top-level comment") || !strings.Contains(out, "# tag comment") {
		t.Fatalf("chart comments not preserved:\n%s", out)
	}
	// The nested override lands at exactly 2-space indent under its parent.
	if !strings.Contains(out, "metrics:\n  enabled: true") {
		t.Fatalf("override not merged at 2-space indent:\n%s", out)
	}
}

// TestMergedNoChartDefaults renders only the catalog overrides when the chart's values are unknown
// (fake mode).
func TestMergedNoChartDefaults(t *testing.T) {
	entry := catalog.Addon{Name: "x", Values: map[string]string{"a.b": "c"}}
	out, err := Merged(entry, "")
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if !strings.Contains(out, "a:") || !strings.Contains(out, "b: c") {
		t.Fatalf("unexpected merged output:\n%s", out)
	}
}

// TestValid rejects malformed YAML and accepts an empty document (reset-to-defaults).
func TestValid(t *testing.T) {
	if err := Valid(""); err != nil {
		t.Fatalf("empty should be valid: %v", err)
	}
	if err := Valid("a: b\n  c: d\n"); err == nil {
		t.Fatalf("expected malformed YAML to be rejected")
	}
	if err := Valid("a:\n  b: 1\n"); err != nil {
		t.Fatalf("valid YAML rejected: %v", err)
	}
}

// TestFakeDefaultsDeterministic checks the fake provider yields stable, parseable output.
func TestFakeDefaultsDeterministic(t *testing.T) {
	entry := catalog.Addon{Name: "trivy-operator", Chart: "trivy-operator", Version: "0.34.0",
		Repo: "https://aquasecurity.github.io/helm-charts", Values: map[string]string{"trivy.ignoreUnfixed": "false"}}
	a, err := Fake{}.Defaults(nil, entry)
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	b, _ := Fake{}.Defaults(nil, entry)
	if a != b {
		t.Fatalf("fake output not deterministic")
	}
	// strip comments; body must be valid YAML
	if err := Valid(a); err != nil {
		t.Fatalf("fake output invalid YAML: %v\n%s", err, a)
	}
}
