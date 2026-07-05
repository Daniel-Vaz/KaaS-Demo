package kube

import (
	"context"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// ExposedApps is the one piece of real logic above the fake/real seam - the derivation the whole
// Networking page is built around - so it is tested directly, on the shapes a real cluster produces.

func gateway(name, ns, addr string, listeners ...GatewayListener) GatewaySummary {
	return GatewaySummary{
		Namespace: ns, Name: name, Class: DefaultGatewayClass,
		Addresses: []string{addr}, Listeners: listeners, Programmed: true,
		IsDefault: IsDefaultGateway(ObjectRef{Namespace: ns, Name: name}),
	}
}

func defaultGateway(appsDomain string) GatewaySummary {
	return gateway(DefaultGatewayName, DefaultGatewayNamespace, "10.10.0.9",
		GatewayListener{Name: "http", Protocol: "HTTP", Port: 80, Programmed: true},
		GatewayListener{
			Name: "https", Protocol: "HTTPS", Port: 443, Hostname: "*." + appsDomain,
			TLSMode: "Terminate", CertificateRefs: []string{"kaas-default-tls"}, Programmed: true,
		},
	)
}

func httpRoute(ns, name string, hosts []string) RouteSummary {
	return RouteSummary{
		Kind: KindHTTPRoute, Namespace: ns, Name: name, Hostnames: hosts,
		ParentRefs: []ParentRef{{
			Namespace: DefaultGatewayNamespace, Name: DefaultGatewayName, Accepted: true,
		}},
		Rules: []RouteRule{{
			Backends: []RouteBackend{{Namespace: ns, Name: name, Kind: "Service", Port: 80}},
		}},
		Accepted: true,
	}
}

// A route for a name under the cluster's apps domain, attached to the default Gateway, is the whole
// platform contract: it must come back as an https URL, marked as covered by the platform's wildcard.
func TestExposedAppsPlatformContract(t *testing.T) {
	const apps = "apps.dev.kaas.example.internal"
	routes := []RouteSummary{httpRoute("demo", "web", []string{"web." + apps})}

	got := ExposedApps(routes, []GatewaySummary{defaultGateway(apps)}, apps)
	if len(got) != 1 {
		t.Fatalf("want 1 exposed app, got %d: %+v", len(got), got)
	}
	a := got[0]
	if a.URL != "https://web."+apps {
		t.Errorf("URL = %q, want https", a.URL)
	}
	if !a.TLS {
		t.Error("TLS = false; the wildcard HTTPS listener covers this hostname")
	}
	if !a.PlatformDomain {
		t.Error("PlatformDomain = false; the hostname is under the cluster's apps domain")
	}
	if a.Address != "10.10.0.9" {
		t.Errorf("Address = %q, want the gateway's", a.Address)
	}
	if !a.Accepted {
		t.Error("Accepted = false on an accepted route+parent")
	}
}

// A hostname the user brought themselves still routes, but it is NOT covered by the platform's
// wildcard record and the TLS listener's hostname doesn't match it - so it is http and "external".
// This is the distinction the page exists to make visible.
func TestExposedAppsForeignHostnameIsNotPlatformDNS(t *testing.T) {
	const apps = "apps.dev.kaas.example.internal"
	routes := []RouteSummary{httpRoute("demo", "shop", []string{"shop.example.com"})}

	got := ExposedApps(routes, []GatewaySummary{defaultGateway(apps)}, apps)
	if len(got) != 1 {
		t.Fatalf("want 1 exposed app, got %d", len(got))
	}
	if got[0].PlatformDomain {
		t.Error("PlatformDomain = true for a hostname outside the apps domain")
	}
	if got[0].TLS {
		t.Error("TLS = true; the HTTPS listener's *.<apps domain> hostname does not cover this name")
	}
	if got[0].URL != "http://shop.example.com" {
		t.Errorf("URL = %q, want http", got[0].URL)
	}
}

// A route naming a Gateway we can't see contributes nothing: without the gateway there is no address
// and no TLS answer, so calling it "exposed" would be a guess.
func TestExposedAppsSkipsUnknownGateway(t *testing.T) {
	r := httpRoute("demo", "web", []string{"web.apps.example"})
	r.ParentRefs = []ParentRef{{Namespace: "other", Name: "someone-elses", Accepted: true}}

	if got := ExposedApps([]RouteSummary{r}, []GatewaySummary{defaultGateway("apps.example")}, "apps.example"); len(got) != 0 {
		t.Fatalf("want no exposed apps for an unresolvable parent, got %+v", got)
	}
}

// A route with no hostnames inherits its listener's - the catch-all shape. Only listeners that HAVE
// a hostname can contribute a URL, so the plaintext listener (hostname "") adds nothing and the
// wildcard HTTPS one supplies the name.
func TestExposedAppsInheritsListenerHostname(t *testing.T) {
	const apps = "apps.dev.kaas.example.internal"
	r := httpRoute("demo", "catchall", nil)

	got := ExposedApps([]RouteSummary{r}, []GatewaySummary{defaultGateway(apps)}, apps)
	if len(got) != 1 {
		t.Fatalf("want 1 exposed app, got %d: %+v", len(got), got)
	}
	if got[0].Hostname != "*."+apps {
		t.Errorf("Hostname = %q, want the listener's wildcard", got[0].Hostname)
	}
	// The URL drops the "*." so it is clickable rather than a literal wildcard.
	if got[0].URL != "https://"+apps {
		t.Errorf("URL = %q, want the wildcard stripped", got[0].URL)
	}
}

// PlatformOverview is the shared filler both backends use, so the contract it renders must match the
// cluster row exactly - the wildcard record in particular is what a user copies into a ticket.
func TestPlatformOverviewRendersTheContract(t *testing.T) {
	c := &domain.Cluster{
		LoadBalancerIP: "10.10.0.9",
		AppsDomain:     "apps.dev.kaas.example.internal",
		DNSDomain:      "dev.kaas.example.internal",
		GatewayWired:   true,
		DNSWired:       true,
		Addons: []domain.Addon{
			{Name: GatewayAddon, Phase: "installed"},
			{Name: MetalLBAddon, Phase: "installed"},
			{Name: CertManagerAddon, Phase: "installing"}, // not installed yet
		},
	}
	ov := PlatformOverview(c)
	if want := "*.apps.dev.kaas.example.internal A 10.10.0.9"; ov.WildcardRecord != want {
		t.Errorf("WildcardRecord = %q, want %q", ov.WildcardRecord, want)
	}
	if !ov.Addons.Gateway || !ov.Addons.MetalLB {
		t.Error("installed add-ons not reported")
	}
	if ov.Addons.CertManager {
		t.Error("CertManager reported installed while still installing")
	}
	if ov.Addons.ExternalDNS {
		t.Error("ExternalDNS reported installed while absent")
	}
}

// A cluster with no apps domain has no wildcard to publish - the record must be empty rather than a
// half-rendered string with a missing name.
func TestPlatformOverviewWithoutAppsDomain(t *testing.T) {
	ov := PlatformOverview(&domain.Cluster{LoadBalancerIP: "10.10.0.9"})
	if ov.WildcardRecord != "" {
		t.Errorf("WildcardRecord = %q, want empty without an apps domain", ov.WildcardRecord)
	}
}

// ---- the fake -----------------------------------------------------------------

func fakeCluster() *domain.Cluster {
	return &domain.Cluster{
		ID: "c-1", Name: "dev", K8sVersion: "1.31.4", CNI: "cilium",
		CreatedAt:      time.Now().Add(-3 * time.Hour),
		LoadBalancerIP: "10.10.0.9",
		AppsDomain:     "apps.dev.kaas.example.internal",
		DNSDomain:      "dev.kaas.example.internal",
		GatewayWired:   true,
		DNSWired:       true,
		Nodes: []domain.Node{
			{VMName: "dev-cp-0", Role: domain.RoleControlPlane, IP: "10.10.0.10"},
			{VMName: "dev-default-0", Role: domain.RoleWorker, IP: "10.10.0.11"},
		},
		Addons: []domain.Addon{
			{Name: GatewayAddon, Phase: "installed"},
			{Name: MetalLBAddon, Phase: "installed"},
			{Name: CertManagerAddon, Phase: "installed"},
		},
	}
}

// The fake must tell the same story the real backend does: the cluster's reserved address held by
// the Envoy Gateway's LoadBalancer Service and by the default Gateway, and demo routes under the
// cluster's own apps domain - otherwise `up-fake` demos something the platform doesn't do.
func TestFakeNetworkOverviewMatchesTheContract(t *testing.T) {
	f := NewFake()
	c := fakeCluster()

	ov, err := f.NetworkOverview(context.Background(), c, nil)
	if err != nil {
		t.Fatalf("NetworkOverview: %v", err)
	}
	if ov.DefaultGateway == nil {
		t.Fatal("no default gateway on a wired cluster")
	}
	if got := ov.DefaultGateway.Addresses; len(got) != 1 || got[0] != c.LoadBalancerIP {
		t.Errorf("default gateway addresses = %v, want the cluster's reserved IP", got)
	}
	if len(ov.DefaultGateway.Listeners) != 2 {
		t.Errorf("want an HTTP and an HTTPS listener with cert-manager installed, got %d",
			len(ov.DefaultGateway.Listeners))
	}
	if len(ov.ExposedApps) == 0 {
		t.Fatal("no exposed apps on a wired cluster with routes")
	}
	for _, a := range ov.ExposedApps {
		if !a.PlatformDomain {
			t.Errorf("exposed app %q is not under the cluster's apps domain", a.Hostname)
		}
		if !a.TLS {
			t.Errorf("exposed app %q is not TLS despite the HTTPS listener", a.Hostname)
		}
	}
	// The Envoy proxy Service must carry the reserved address, since that is where MetalLB puts it.
	found := false
	for _, s := range ov.LoadBalancerServices {
		for _, ip := range s.ExternalIPs {
			if ip == c.LoadBalancerIP {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no load-balanced service holds %s: %+v", c.LoadBalancerIP, ov.LoadBalancerServices)
	}
}

// Deselecting envoy-gateway is legitimate, and must empty the Gateway API views rather than error -
// the platform half of the overview still applies (the address stays reserved).
func TestFakeNetworkWithoutGatewayAddon(t *testing.T) {
	f := NewFake()
	c := fakeCluster()
	c.Addons = []domain.Addon{{Name: MetalLBAddon, Phase: "installed"}}

	ov, err := f.NetworkOverview(context.Background(), c, nil)
	if err != nil {
		t.Fatalf("NetworkOverview: %v", err)
	}
	if ov.DefaultGateway != nil {
		t.Error("default gateway present without the envoy-gateway add-on")
	}
	if len(ov.ExposedApps) != 0 {
		t.Errorf("exposed apps without a gateway: %+v", ov.ExposedApps)
	}
	if ov.LoadBalancerIP != c.LoadBalancerIP {
		t.Error("the reserved address is still the cluster's, add-on or not")
	}
	if ov.Addons.Gateway {
		t.Error("Addons.Gateway true without the add-on")
	}
}

// Services and their detail must agree - the drawer's endpoint list is what the list's count claims.
func TestFakeServiceDetail(t *testing.T) {
	f := NewFake()
	c := fakeCluster()
	ctx := context.Background()

	svcs, err := f.Services(ctx, c, nil, "demo")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(svcs) == 0 {
		t.Fatal("no services in the demo namespace")
	}
	for _, s := range svcs {
		d, err := f.Service(ctx, c, nil, ObjectRef{Namespace: s.Namespace, Name: s.Name})
		if err != nil {
			t.Fatalf("Service %s/%s: %v", s.Namespace, s.Name, err)
		}
		if len(d.Backends) != s.Endpoints {
			t.Errorf("%s/%s: %d backends but the list says %d endpoints",
				s.Namespace, s.Name, len(d.Backends), s.Endpoints)
		}
		if _, err := f.ServiceManifest(ctx, c, nil, ObjectRef{Namespace: s.Namespace, Name: s.Name}); err != nil {
			t.Errorf("ServiceManifest %s/%s: %v", s.Namespace, s.Name, err)
		}
	}
}
