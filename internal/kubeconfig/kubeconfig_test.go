package kubeconfig

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sample = `apiVersion: v1
kind: Config
clusters:
- name: kaas
  cluster:
    server: https://10.200.3.10:6443
    certificate-authority-data: QUJD
contexts:
- name: kaas
  context:
    cluster: kaas
    user: kaas-admin
current-context: kaas
users:
- name: kaas-admin
  user:
    client-certificate-data: QUJD
`

// A local KVM host must leave the kubeconfig byte-for-byte alone: no proxy, no re-serialisation.
func TestWithProxyEmptyIsIdentity(t *testing.T) {
	out, err := WithProxy([]byte(sample), "")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != sample {
		t.Fatalf("kubeconfig was rewritten with no proxy configured:\n%s", out)
	}
}

// The proxy lands on the cluster entry (where client-go reads it) and nothing else is disturbed -
// the server address in particular stays as-is, since it is resolved on the far side of the tunnel.
func TestWithProxySetsProxyURL(t *testing.T) {
	out, err := WithProxy([]byte(sample), "socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Clusters []struct {
			Name    string `yaml:"name"`
			Cluster struct {
				Server   string `yaml:"server"`
				ProxyURL string `yaml:"proxy-url"`
				CAData   string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		CurrentContext string `yaml:"current-context"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Clusters) != 1 {
		t.Fatalf("got %d clusters, want 1", len(doc.Clusters))
	}
	c := doc.Clusters[0].Cluster
	if c.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy-url = %q, want the SOCKS proxy", c.ProxyURL)
	}
	if c.Server != "https://10.200.3.10:6443" {
		t.Errorf("server = %q, want it untouched", c.Server)
	}
	if c.CAData == "" || doc.CurrentContext != "kaas" {
		t.Errorf("unrelated fields lost: ca=%q current-context=%q", c.CAData, doc.CurrentContext)
	}
}

// A kubeconfig with nothing to attach the proxy to would silently produce direct (unroutable)
// connections, so it must fail loudly instead.
func TestWithProxyRejectsClusterlessConfig(t *testing.T) {
	_, err := WithProxy([]byte("apiVersion: v1\nkind: Config\n"), "socks5://127.0.0.1:1080")
	if err == nil || !strings.Contains(err.Error(), "no clusters") {
		t.Fatalf("err = %v, want a no-clusters failure", err)
	}
}
