package kube

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Network types for the portal's Networking page - Services, Gateway API Gateways and Routes, and
// the derived "exposed apps" view over them.
//
// Like the storage half (storage.go) this lives in the kube seam rather than a seam of its own:
// Services are core Kubernetes objects, and Gateways/HTTPRoutes are ordinary CRDs read with the same
// `kubectl get -o json` against the same cluster over the same worker exec agent. So the same Client
// interface, the same transport, and the same view-scoped auth cover them.
//
// The page's reason for existing is the *platform* contract, not the raw objects: every cluster ships
// a default MetalLB pool + Envoy Gateway on a reserved address (Cluster.LoadBalancerIP) and a wildcard
// DNS record for its apps domain (see the default_gateway ansible role and internal/dns). Overview
// renders that contract - the external IP, the wildcard, whether both are wired - and ExposedApps
// answers the question it exists for: what is actually reachable from outside this cluster, at what
// URL. Everything here is strictly read-only.
//
// Gateway API CRDs are installed by the envoy-gateway add-on, so they may be absent. A missing CRD is
// NOT an error: the readers return an empty list, so the Services tab and the Overview's platform half
// still work on a cluster whose user deselected the add-on.

// Gateway API group/kinds. The default Gateway and its class are named by the default_gateway role;
// the platform recognises its own objects by these names so the Overview can single out "the
// cluster's default gateway" from any a user adds later.
const (
	// DefaultGatewayName / DefaultGatewayNamespace / DefaultGatewayClass mirror the CRs the
	// default_gateway ansible role applies. Kept in sync by hand - the role is the authority.
	DefaultGatewayName      = "eg"
	DefaultGatewayNamespace = "envoy-gateway-system"
	DefaultGatewayClass     = "eg"

	// GatewayAddon / MetalLBAddon / CertManagerAddon / ExternalDNSAddon are the catalog add-on names
	// the platform's north-south path is made of. The Overview reports which are present so a user who
	// deselected one sees why their cluster has no external address rather than an empty page.
	GatewayAddon     = "envoy-gateway"
	MetalLBAddon     = "metallb"
	CertManagerAddon = "cert-manager"
	ExternalDNSAddon = "external-dns"
)

// RouteKind is one of the Gateway API route kinds the page lists. The lowercase value is the wire
// form used in URLs and JSON, and is also the plural kubectl resource with an "s" appended.
type RouteKind string

const (
	KindHTTPRoute RouteKind = "httproute"
	KindGRPCRoute RouteKind = "grpcroute"
	KindTCPRoute  RouteKind = "tcproute"
	KindTLSRoute  RouteKind = "tlsroute"
	KindUDPRoute  RouteKind = "udproute"
)

// AllRouteKinds is the order the page lists and groups routes in. HTTPRoute first: it is the kind the
// platform's own contract is written in (attach one to the default Gateway and it is reachable), and
// in practice the only one most clusters use.
var AllRouteKinds = []RouteKind{KindHTTPRoute, KindGRPCRoute, KindTCPRoute, KindTLSRoute, KindUDPRoute}

// Resource returns the plural kubectl resource name for the kind, qualified with the Gateway API
// group so it can never collide with a same-named resource in another group.
func (k RouteKind) Resource() string { return string(k) + "s.gateway.networking.k8s.io" }

// ParseRouteKind normalizes a URL form (singular, plural, or Kind) to a RouteKind.
func ParseRouteKind(s string) (RouteKind, bool) {
	switch RouteKind(strings.ToLower(strings.TrimSuffix(s, "s"))) {
	case KindHTTPRoute:
		return KindHTTPRoute, true
	case KindGRPCRoute:
		return KindGRPCRoute, true
	case KindTCPRoute:
		return KindTCPRoute, true
	case KindTLSRoute:
		return KindTLSRoute, true
	case KindUDPRoute:
		return KindUDPRoute, true
	default:
		return "", false
	}
}

// ObjectRef identifies a namespaced object (a Service, Gateway or Route) within a cluster.
type ObjectRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ServicePort is one port a Service publishes. TargetPort is rendered as a string because it may be
// a named port rather than a number; NodePort is 0 unless the Service type allocates one.
type ServicePort struct {
	Name       string `json:"name,omitempty"`
	Protocol   string `json:"protocol,omitempty"` // TCP | UDP | SCTP
	Port       int    `json:"port"`
	TargetPort string `json:"target_port,omitempty"`
	NodePort   int    `json:"node_port,omitempty"`
	AppProto   string `json:"app_protocol,omitempty"`
}

// ServiceSummary is one row on the Networking page's Services tab. ExternalIPs merges the
// LoadBalancer-assigned ingress addresses with any spec.externalIPs, because from a user's point of
// view both are "where this Service answers from outside" - and on this platform the interesting one
// is MetalLB's, handed to the Envoy Gateway's Service from the cluster's single-address pool.
type ServiceSummary struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Type        string            `json:"type"` // ClusterIP | NodePort | LoadBalancer | ExternalName
	ClusterIP   string            `json:"cluster_ip,omitempty"`
	ExternalIPs []string          `json:"external_ips,omitempty"`
	Ports       []ServicePort     `json:"ports"`
	Selector    map[string]string `json:"selector,omitempty"`
	// Endpoints is the number of ready backing endpoints, the field that tells a "why is this 503"
	// story at a glance. -1 means "not determined" (the endpoint lookup failed or was skipped).
	Endpoints int       `json:"endpoints"`
	CreatedAt time.Time `json:"created_at"`
}

// ServiceDetail is a Service's full view: the summary plus its metadata and the pods behind it.
type ServiceDetail struct {
	ServiceSummary
	Labels                map[string]string `json:"labels,omitempty"`
	Annotations           map[string]string `json:"annotations,omitempty"`
	SessionAffinity       string            `json:"session_affinity,omitempty"`
	ExternalTrafficPolicy string            `json:"external_traffic_policy,omitempty"`
	IPFamilies            []string          `json:"ip_families,omitempty"`
	ExternalName          string            `json:"external_name,omitempty"` // type=ExternalName only
	// Backends are the ready endpoints behind the Service - pod name + IP where the endpoint names a
	// pod. Best-effort: a failed lookup leaves it empty rather than making the Service unviewable.
	Backends []ServiceBackend `json:"backends"`
}

// ServiceBackend is one ready endpoint behind a Service.
type ServiceBackend struct {
	Pod  string `json:"pod,omitempty"`
	IP   string `json:"ip"`
	Node string `json:"node,omitempty"`
}

// GatewayListener is one listener on a Gateway. TLSMode is "" for a plaintext listener; for a
// terminating one it is "Terminate" (or "Passthrough") and CertificateRefs names the Secrets it
// serves - on the platform's default Gateway, the self-signed wildcard cert-manager issues.
type GatewayListener struct {
	Name            string   `json:"name"`
	Protocol        string   `json:"protocol"` // HTTP | HTTPS | TCP | TLS | UDP
	Port            int      `json:"port"`
	Hostname        string   `json:"hostname,omitempty"`
	TLSMode         string   `json:"tls_mode,omitempty"`
	CertificateRefs []string `json:"certificate_refs,omitempty"`
	AttachedRoutes  int      `json:"attached_routes"`
	Programmed      bool     `json:"programmed"`
	Status          string   `json:"status,omitempty"` // a reason when not programmed
}

// GatewaySummary is one row on the Gateways tab, and the whole of a Gateway's detail - a Gateway is
// small enough that the list carries everything the drawer shows besides its YAML.
//
// IsDefault marks the platform's own Gateway (the one the default_gateway role applies and every
// tenant is told to attach routes to), so the UI can say which one the contract is about.
type GatewaySummary struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Class      string            `json:"class"`
	Addresses  []string          `json:"addresses,omitempty"`
	Listeners  []GatewayListener `json:"listeners"`
	Programmed bool              `json:"programmed"`
	Status     string            `json:"status,omitempty"` // a reason when not programmed/accepted
	IsDefault  bool              `json:"is_default,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

// ParentRef is a route's reference to the Gateway (or listener) it attaches to. Namespace is
// resolved to the route's own namespace when the ref omits it, as the Gateway API specifies, so the
// UI never has to re-derive it.
type ParentRef struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	SectionName string `json:"section_name,omitempty"` // the listener, when pinned
	Port        int    `json:"port,omitempty"`
	Accepted    bool   `json:"accepted"`
	Status      string `json:"status,omitempty"` // the not-accepted reason (NoMatchingListenerHostname, …)
}

// RouteBackend is one backend a route rule forwards to.
type RouteBackend struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"` // Service unless the route says otherwise
	Port      int    `json:"port,omitempty"`
	Weight    int    `json:"weight,omitempty"`
}

// RouteRule is one rule of a route: how requests are matched, and where they go. Matches is rendered
// server-side into short human forms ("PathPrefix: /api", "Header foo=bar") because the Gateway API's
// match union is large and only its gist belongs in a table.
type RouteRule struct {
	Matches  []string       `json:"matches,omitempty"`
	Backends []RouteBackend `json:"backends"`
}

// RouteSummary is one row on the Routes tab and the whole of a route's detail besides its YAML.
type RouteSummary struct {
	Kind       RouteKind         `json:"kind"`
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Hostnames  []string          `json:"hostnames,omitempty"`
	ParentRefs []ParentRef       `json:"parent_refs"`
	Rules      []RouteRule       `json:"rules"`
	Accepted   bool              `json:"accepted"`
	Status     string            `json:"status,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

// ExposedApp is one externally reachable application, derived from a route attached to a Gateway -
// the answer to "what can I reach on this cluster from outside, and at what URL". It is computed
// server-side (ExposedApps below) rather than in the portal so the fake, the real backend and any
// future client all agree on what "exposed" means.
type ExposedApp struct {
	Hostname string    `json:"hostname"`
	URL      string    `json:"url"` // https:// when a matching TLS listener exists, else http://
	TLS      bool      `json:"tls"`
	Route    ObjectRef `json:"route"`
	// RouteKind is carried alongside Route because the manifest endpoint is keyed on it.
	RouteKind RouteKind `json:"route_kind"`
	Gateway   ObjectRef `json:"gateway"`
	// Address is the gateway address the hostname resolves to - the cluster's reserved
	// LoadBalancerIP for the default Gateway.
	Address string `json:"address,omitempty"`
	// Backends are the route's backends, flattened, so the row says what actually serves the name.
	Backends []RouteBackend `json:"backends"`
	// PlatformDomain reports that the hostname falls under the cluster's apps domain, i.e. it is
	// covered by the platform's wildcard DNS record and needs no DNS of the user's own. A false here
	// on an otherwise healthy route is the "why doesn't my name resolve" answer.
	PlatformDomain bool   `json:"platform_domain"`
	Accepted       bool   `json:"accepted"`
	Status         string `json:"status,omitempty"`
}

// NetworkOverview is the Networking page's Overview: the cluster's default north-south contract
// (the reserved gateway address, the wildcard DNS record, whether both are wired) plus what is
// actually published through it.
//
// The platform half comes from the cluster row and is filled by PlatformOverview below - shared by
// the real and fake backends so the two can't drift. The live half comes from the cluster.
type NetworkOverview struct {
	// --- the platform contract, from the cluster row (see internal/domain) ---

	// LoadBalancerIP is the address reserved for this cluster's MetalLB pool, which the default
	// Envoy Gateway holds. Empty when the deployment reserves none.
	LoadBalancerIP string `json:"load_balancer_ip,omitempty"`
	// AppsDomain / DNSDomain are the cluster's admission-derived domains, empty when the deployment
	// runs without KAAS_DNS_BASE_DOMAIN.
	AppsDomain string `json:"apps_domain,omitempty"`
	DNSDomain  string `json:"dns_domain,omitempty"`
	// WildcardRecord is the one DNS record the platform publishes for this cluster, rendered as it
	// would appear in the zone ("*.apps.foo.example A 10.0.0.5"). Empty when there is no apps domain.
	WildcardRecord string `json:"wildcard_record,omitempty"`
	// GatewayWired / DNSWired mirror the reconciler's one-shot markers: the CRs have been applied,
	// and the wildcard has been published. Both false on a cluster still converging.
	GatewayWired bool `json:"gateway_wired"`
	DNSWired     bool `json:"dns_wired"`
	// Addons reports which of the north-south add-ons are installed, so a page that is empty because
	// the user deselected envoy-gateway says so instead of looking broken.
	Addons NetworkAddons `json:"addons"`

	// --- what is live in the cluster ---

	// DefaultGateway is the platform's own Gateway, nil when the CRs aren't applied (or the add-on
	// isn't installed).
	DefaultGateway *GatewaySummary `json:"default_gateway,omitempty"`
	// ExposedApps is every hostname reachable through a Gateway, newest-sorted by hostname.
	ExposedApps []ExposedApp `json:"exposed_apps"`
	// LoadBalancerServices are the Services holding an external address - the Envoy Gateway's own,
	// plus any a user creates directly. The second, non-platform way out of the cluster.
	LoadBalancerServices []ServiceSummary `json:"load_balancer_services"`
	// Counts for the page's header tiles, computed server-side so every tab agrees.
	ServiceCount int `json:"service_count"`
	RouteCount   int `json:"route_count"`
	GatewayCount int `json:"gateway_count"`
}

// NetworkAddons reports which north-south add-ons are installed on the cluster.
type NetworkAddons struct {
	Gateway     bool `json:"gateway"`      // envoy-gateway
	MetalLB     bool `json:"metallb"`      // metallb
	CertManager bool `json:"cert_manager"` // cert-manager (the HTTPS listener's issuer)
	ExternalDNS bool `json:"external_dns"` // external-dns (the user's half of DNS)
}

// PlatformOverview fills the platform half of an Overview from the cluster row. Both backends call
// it, so the real and fake pages report the contract identically - the difference between them is
// only what they can see *inside* the cluster.
func PlatformOverview(c *domain.Cluster) NetworkOverview {
	ov := NetworkOverview{
		LoadBalancerIP: c.LoadBalancerIP,
		AppsDomain:     c.AppsDomain,
		DNSDomain:      c.DNSDomain,
		GatewayWired:   c.GatewayWired,
		DNSWired:       c.DNSWired,
		Addons: NetworkAddons{
			Gateway:     addonInstalled(c, GatewayAddon),
			MetalLB:     addonInstalled(c, MetalLBAddon),
			CertManager: addonInstalled(c, CertManagerAddon),
			ExternalDNS: addonInstalled(c, ExternalDNSAddon),
		},
		ExposedApps:          []ExposedApp{},
		LoadBalancerServices: []ServiceSummary{},
	}
	if c.AppsDomain != "" && c.LoadBalancerIP != "" {
		ov.WildcardRecord = "*." + c.AppsDomain + " A " + c.LoadBalancerIP
	}
	return ov
}

// addonInstalled reports whether the named add-on is installed on the cluster (mirrors
// monitoring.Enabled / security.Enabled, kept local so the kube seam imports neither).
func addonInstalled(c *domain.Cluster, name string) bool {
	for _, a := range c.Addons {
		if a.Name == name && a.Phase == "installed" {
			return true
		}
	}
	return false
}

// IsDefaultGateway reports whether a Gateway is the one the platform applies for the cluster.
func IsDefaultGateway(ref ObjectRef) bool {
	return ref.Namespace == DefaultGatewayNamespace && ref.Name == DefaultGatewayName
}

// ExposedApps derives the externally reachable applications from a cluster's routes and gateways.
// It is the page's headline view and the one piece of real logic in this file, so it lives here -
// above the fake/real seam - and both backends call it with whatever they can see.
//
// A route contributes one entry per hostname, per gateway it is accepted by. A route with no
// hostnames (a catch-all attached to the default Gateway) contributes one entry under the gateway's
// own listener hostname where it has one, and is otherwise skipped: a URL is the point of the view,
// and "no hostname on any listener" means there is nothing to click.
func ExposedApps(routes []RouteSummary, gateways []GatewaySummary, appsDomain string) []ExposedApp {
	byRef := make(map[ObjectRef]GatewaySummary, len(gateways))
	for _, g := range gateways {
		byRef[ObjectRef{Namespace: g.Namespace, Name: g.Name}] = g
	}

	var out []ExposedApp
	seen := map[string]bool{} // hostname|route|gateway - a route may name a gateway twice
	for _, r := range routes {
		var backends []RouteBackend
		for _, rule := range r.Rules {
			backends = append(backends, rule.Backends...)
		}
		for _, p := range r.ParentRefs {
			gref := ObjectRef{Namespace: p.Namespace, Name: p.Name}
			g, ok := byRef[gref]
			if !ok {
				// A parent we can't see (a Gateway in a namespace the read didn't cover) still names a
				// real attachment, but we can't say its address or TLS, so it isn't an "exposed app".
				continue
			}
			hosts := r.Hostnames
			if len(hosts) == 0 {
				hosts = listenerHostnames(g, p.SectionName)
			}
			for _, h := range hosts {
				key := h + "|" + r.Namespace + "/" + r.Name + "|" + gref.Namespace + "/" + gref.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				tls := gatewayTerminatesTLS(g, h, p.SectionName)
				scheme := "http://"
				if tls {
					scheme = "https://"
				}
				out = append(out, ExposedApp{
					Hostname:       h,
					URL:            scheme + strings.TrimPrefix(h, "*."),
					TLS:            tls,
					Route:          ObjectRef{Namespace: r.Namespace, Name: r.Name},
					RouteKind:      r.Kind,
					Gateway:        gref,
					Address:        firstAddress(g),
					Backends:       backends,
					PlatformDomain: underDomain(h, appsDomain),
					Accepted:       p.Accepted && r.Accepted,
					Status:         p.Status,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hostname != out[j].Hostname {
			return out[i].Hostname < out[j].Hostname
		}
		return out[i].Route.Name < out[j].Route.Name
	})
	return out
}

// listenerHostnames returns the hostnames a hostname-less route inherits from its gateway: the
// pinned listener's, or every listener's when the route pins none.
func listenerHostnames(g GatewaySummary, section string) []string {
	var hs []string
	for _, l := range g.Listeners {
		if section != "" && l.Name != section {
			continue
		}
		if l.Hostname != "" {
			hs = append(hs, l.Hostname)
		}
	}
	return hs
}

// gatewayTerminatesTLS reports whether some listener on the gateway terminates TLS for hostname -
// i.e. whether the app answers on https. A listener with no hostname serves every name.
func gatewayTerminatesTLS(g GatewaySummary, hostname, section string) bool {
	for _, l := range g.Listeners {
		if section != "" && l.Name != section {
			continue
		}
		if l.TLSMode == "" && !strings.EqualFold(l.Protocol, "HTTPS") {
			continue
		}
		if l.Hostname == "" || hostnameMatches(l.Hostname, hostname) {
			return true
		}
	}
	return false
}

// hostnameMatches implements the Gateway API's hostname intersection for the one case that matters
// here: a listener wildcard ("*.apps.foo") covering a concrete route hostname, plus exact equality.
func hostnameMatches(listener, host string) bool {
	if listener == host {
		return true
	}
	if suffix, ok := strings.CutPrefix(listener, "*."); ok {
		return strings.HasSuffix(host, "."+suffix) || host == suffix || host == "*."+suffix
	}
	return false
}

// underDomain reports whether host is covered by the cluster's apps domain - and so by the wildcard
// record the platform publishes.
func underDomain(host, domain string) bool {
	if domain == "" || host == "" {
		return false
	}
	return host == domain || host == "*."+domain || strings.HasSuffix(host, "."+domain)
}

func firstAddress(g GatewaySummary) string {
	if len(g.Addresses) > 0 {
		return g.Addresses[0]
	}
	return ""
}

// NetworkReader reads a Ready cluster's networking objects. It is part of Client (see kube.go);
// split into its own interface purely to keep the page-shaped surfaces readable. namespace == ""
// means "all namespaces". Every method is read-only, so view access suffices and a read-role
// member's per-user reader credential serves them all (see the viewer_kubeconfig ansible role,
// which grants the Gateway API group - the built-in `view` role does not cover CRDs).
type NetworkReader interface {
	// NetworkOverview returns the cluster's north-south contract and what is published through it.
	NetworkOverview(ctx context.Context, c *domain.Cluster, kubeconfig []byte) (*NetworkOverview, error)
	// Services lists the cluster's Services as summary rows.
	Services(ctx context.Context, c *domain.Cluster, kubeconfig []byte, namespace string) ([]ServiceSummary, error)
	// Service returns one Service's detail, including the endpoints behind it.
	Service(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref ObjectRef) (*ServiceDetail, error)
	// ServiceManifest returns a Service's YAML.
	ServiceManifest(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref ObjectRef) (string, error)
	// ServiceEvents returns the events for a Service (a LoadBalancer with no address lands here).
	ServiceEvents(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref ObjectRef) ([]Event, error)
	// Gateways lists every Gateway in the cluster. An absent Gateway API CRD yields an empty list,
	// not an error - the add-on is optional.
	Gateways(ctx context.Context, c *domain.Cluster, kubeconfig []byte) ([]GatewaySummary, error)
	// GatewayManifest returns a Gateway's YAML.
	GatewayManifest(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref ObjectRef) (string, error)
	// Routes lists the cluster's Gateway API routes of every kind. An absent CRD yields no rows of
	// that kind rather than an error.
	Routes(ctx context.Context, c *domain.Cluster, kubeconfig []byte, namespace string) ([]RouteSummary, error)
	// RouteManifest returns a route's YAML.
	RouteManifest(ctx context.Context, c *domain.Cluster, kubeconfig []byte, kind RouteKind, ref ObjectRef) (string, error)
}
