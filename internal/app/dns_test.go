package app

import (
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/dns"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func labDNS(t *testing.T) dns.Settings {
	t.Helper()
	s, err := dns.Settings{
		BaseDomain:  "kaas.example.internal",
		Server:      "dc01.example.internal",
		Auth:        dns.AuthGSS,
		KrbUsername: "svc-kaas",
		KrbRealm:    "EXAMPLE.INTERNAL",
		KrbPassword: "hunter2",
	}.ValidateUpdate()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Admission stamps the cluster's domains onto the row, so they survive a later change to the
// deployment's base domain.
func TestCreateClusterStampsDNSDomains(t *testing.T) {
	a := newTenancyApp(t)
	a.dns = labDNS(t)
	c, err := a.CreateCluster(admin(t, a), CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DNSDomain != "dev.kaas.example.internal" || c.AppsDomain != "apps.dev.kaas.example.internal" {
		t.Fatalf("domains = %q / %q", c.DNSDomain, c.AppsDomain)
	}
	stored, err := a.Store.GetCluster(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AppsDomain != c.AppsDomain {
		t.Fatalf("stored apps domain = %q", stored.AppsDomain)
	}
}

// With no site DNS configured a cluster is simply admitted without a domain - no error, no name.
func TestCreateClusterWithoutDNS(t *testing.T) {
	a := newTenancyApp(t)
	c, err := a.CreateCluster(admin(t, a), CreateRequest{Name: "dev", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DNSDomain != "" || c.AppsDomain != "" {
		t.Fatalf("domains = %q / %q, want empty", c.DNSDomain, c.AppsDomain)
	}
}

func TestExternalDNSExtras(t *testing.T) {
	extras := dnsAddonExtras(labDNS(t))
	c := &domain.Cluster{
		ID: "c1", Name: "dev",
		DNSDomain: "dev.kaas.example.internal", AppsDomain: "apps.dev.kaas.example.internal",
		Addons: []domain.Addon{{Name: "envoy-gateway"}, {Name: externalDNSAddon}},
	}

	// Another add-on gets nothing: this hook is external-dns's alone.
	if got := extras(c, domain.Addon{Name: "metrics-server"}); got.Values != nil || got.Secret != nil {
		t.Fatalf("metrics-server got extras: %+v", got)
	}

	got := extras(c, domain.Addon{Name: externalDNSAddon})
	want := map[string]string{
		"provider.name":                       "rfc2136",
		"domainFilters[0]":                    "dev.kaas.example.internal",
		"extraArgs.rfc2136-host":              "dc01.example.internal",
		"extraArgs.rfc2136-zone":              "kaas.example.internal",
		"extraArgs.rfc2136-gss-tsig":          "true",
		"extraArgs.rfc2136-kerberos-realm":    "EXAMPLE.INTERNAL",
		"extraArgs.rfc2136-kerberos-username": "svc-kaas",
		"sources[2]":                          "gateway-httproute",
	}
	for k, v := range want {
		if got.Values[k] != v {
			t.Fatalf("value %s = %q, want %q", k, got.Values[k], v)
		}
	}
	// The credential rides in a Secret, never in a Helm value - a value would land in the
	// Deployment's env, readable by anyone in the cluster who can read a pod spec.
	if got.Secret == nil || got.Secret.Data["password"] != "hunter2" {
		t.Fatalf("credential secret = %+v", got.Secret)
	}
	for k, v := range got.Values {
		if v == "hunter2" {
			t.Fatalf("credential leaked into helm value %q", k)
		}
	}

	// No Gateway API in the cluster → no gateway-httproute source, or external-dns fails its
	// initial sync on a CRD that isn't there.
	c.Addons = []domain.Addon{{Name: externalDNSAddon}}
	if got := extras(c, domain.Addon{Name: externalDNSAddon}); got.Values["sources[2]"] != "" {
		t.Fatalf("gateway source configured without envoy-gateway: %q", got.Values["sources[2]"])
	}
}

// With no site DNS the add-on still installs, on the catalog's inert inmemory provider.
func TestExternalDNSExtrasWithoutDNS(t *testing.T) {
	got := dnsAddonExtras(dns.Settings{})(&domain.Cluster{ID: "c1", Name: "dev"}, domain.Addon{Name: externalDNSAddon})
	if got.Secret != nil {
		t.Fatalf("secret created with no DNS configured: %+v", got.Secret)
	}
	if got.Values["provider.name"] != "" {
		t.Fatalf("provider overridden with no DNS configured: %q", got.Values["provider.name"])
	}
}
