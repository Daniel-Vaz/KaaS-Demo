package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// The kube.NetworkReader half of the Fake: a plausible set of Services, Gateways and HTTPRoutes
// synthesized from control-plane state, so the Networking page is demoable with no KVM. Deterministic
// in cluster state, like the workload and storage fakes, so the portal's polling doesn't make the
// page flicker.
//
// It is built to tell the platform's story specifically: the cluster's reserved LoadBalancerIP shows
// up as the Envoy Gateway Service's external address and as the default Gateway's, and the demo
// HTTPRoutes carry hostnames under the cluster's own apps domain - so ExposedApps produces exactly
// the rows a real cluster would. Everything is gated on the same add-ons the real thing needs, so
// deselecting envoy-gateway empties the Gateways/Routes tabs here too.

func (f *Fake) Services(_ context.Context, c *domain.Cluster, _ []byte, namespace string) ([]ServiceSummary, error) {
	out := []ServiceSummary{}
	for _, s := range f.buildServices(c) {
		if namespace != "" && s.Namespace != namespace {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *Fake) Service(_ context.Context, c *domain.Cluster, _ []byte, ref ObjectRef) (*ServiceDetail, error) {
	s, ok := f.findService(c, ref)
	if !ok {
		return nil, fmt.Errorf("service %s/%s not found", ref.Namespace, ref.Name)
	}
	d := ServiceDetail{
		ServiceSummary:  s,
		Labels:          map[string]string{"app.kubernetes.io/name": s.Name},
		SessionAffinity: "None",
		IPFamilies:      []string{"IPv4"},
		Backends:        f.serviceBackends(c, s),
	}
	if s.Type == "LoadBalancer" {
		d.ExternalTrafficPolicy = "Cluster"
	}
	return &d, nil
}

func (f *Fake) ServiceManifest(_ context.Context, c *domain.Cluster, _ []byte, ref ObjectRef) (string, error) {
	s, ok := f.findService(c, ref)
	if !ok {
		return "", fmt.Errorf("service %s/%s not found", ref.Namespace, ref.Name)
	}
	return f.serviceManifest(s), nil
}

func (f *Fake) ServiceEvents(_ context.Context, c *domain.Cluster, _ []byte, ref ObjectRef) ([]Event, error) {
	s, ok := f.findService(c, ref)
	if !ok {
		return nil, fmt.Errorf("service %s/%s not found", ref.Namespace, ref.Name)
	}
	// Only a LoadBalancer has anything interesting to say - MetalLB's assignment. Everything else
	// legitimately has no events, which is itself accurate.
	if s.Type != "LoadBalancer" || len(s.ExternalIPs) == 0 {
		return []Event{}, nil
	}
	return []Event{{
		Type: "Normal", Reason: "IPAllocated", Count: 1,
		Message:  "Assigned IP [\"" + s.ExternalIPs[0] + "\"]",
		LastSeen: f.clusterEpoch(c).Add(30 * time.Minute),
		Object:   "Service/" + s.Name,
	}}, nil
}

func (f *Fake) Gateways(_ context.Context, c *domain.Cluster, _ []byte) ([]GatewaySummary, error) {
	return f.buildGateways(c), nil
}

func (f *Fake) GatewayManifest(_ context.Context, c *domain.Cluster, _ []byte, ref ObjectRef) (string, error) {
	for _, g := range f.buildGateways(c) {
		if g.Namespace == ref.Namespace && g.Name == ref.Name {
			return f.gatewayManifest(g), nil
		}
	}
	return "", fmt.Errorf("gateway %s/%s not found", ref.Namespace, ref.Name)
}

func (f *Fake) Routes(_ context.Context, c *domain.Cluster, _ []byte, namespace string) ([]RouteSummary, error) {
	out := []RouteSummary{}
	for _, r := range f.buildRoutes(c) {
		if namespace != "" && r.Namespace != namespace {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *Fake) RouteManifest(_ context.Context, c *domain.Cluster, _ []byte, kind RouteKind, ref ObjectRef) (string, error) {
	for _, r := range f.buildRoutes(c) {
		if r.Kind == kind && r.Namespace == ref.Namespace && r.Name == ref.Name {
			return f.routeManifest(r), nil
		}
	}
	return "", fmt.Errorf("%s %s/%s not found", kind, ref.Namespace, ref.Name)
}

func (f *Fake) NetworkOverview(_ context.Context, c *domain.Cluster, _ []byte) (*NetworkOverview, error) {
	ov := PlatformOverview(c)
	svcs := f.buildServices(c)
	gws := f.buildGateways(c)
	routes := f.buildRoutes(c)

	ov.ServiceCount = len(svcs)
	ov.GatewayCount = len(gws)
	ov.RouteCount = len(routes)
	for _, s := range svcs {
		if s.Type == "LoadBalancer" || len(s.ExternalIPs) > 0 {
			ov.LoadBalancerServices = append(ov.LoadBalancerServices, s)
		}
	}
	for i := range gws {
		if gws[i].IsDefault {
			g := gws[i]
			ov.DefaultGateway = &g
			break
		}
	}
	if apps := ExposedApps(routes, gws, c.AppsDomain); len(apps) > 0 {
		ov.ExposedApps = apps
	}
	return &ov, nil
}

// ---- synthesized networking model ----------------------------------------------

// clusterEpoch is a stable "when this cluster's objects were made" anchor, so creation timestamps
// are deterministic per cluster rather than moving on every poll.
func (f *Fake) clusterEpoch(c *domain.Cluster) time.Time {
	if !c.CreatedAt.IsZero() {
		return c.CreatedAt
	}
	return time.Now().Add(-4 * time.Hour)
}

// buildServices returns the synthesized Service set: the ones every kubeadm cluster has, one per
// pod-bearing add-on that publishes a Service, and Services for the demo workloads the workload fake
// builds - so the two pages describe the same cluster.
func (f *Fake) buildServices(c *domain.Cluster) []ServiceSummary {
	born := f.clusterEpoch(c)
	tcp := func(port int, target string) []ServicePort {
		return []ServicePort{{Name: "http", Protocol: "TCP", Port: port, TargetPort: target}}
	}
	svc := func(ns, name, typ, ip string, ports []ServicePort, sel map[string]string, eps int) ServiceSummary {
		return ServiceSummary{
			Namespace: ns, Name: name, Type: typ, ClusterIP: ip, Ports: ports,
			Selector: sel, Endpoints: eps, CreatedAt: born,
		}
	}

	out := []ServiceSummary{
		svc("default", "kubernetes", "ClusterIP", "10.96.0.1",
			[]ServicePort{{Name: "https", Protocol: "TCP", Port: 443, TargetPort: "6443"}},
			nil, len(controlPlaneNodes(c))),
		svc("kube-system", "kube-dns", "ClusterIP", "10.96.0.10",
			[]ServicePort{
				{Name: "dns", Protocol: "UDP", Port: 53, TargetPort: "53"},
				{Name: "dns-tcp", Protocol: "TCP", Port: 53, TargetPort: "53"},
				{Name: "metrics", Protocol: "TCP", Port: 9153, TargetPort: "9153"},
			},
			map[string]string{"k8s-app": "kube-dns"}, 2),
	}

	ip := clusterIPFn(11)
	for _, a := range c.Addons {
		if a.Phase == "removing" {
			continue
		}
		switch a.Name {
		case "metrics-server":
			out = append(out, svc("kube-system", "metrics-server", "ClusterIP", ip(),
				[]ServicePort{{Name: "https", Protocol: "TCP", Port: 443, TargetPort: "https"}},
				map[string]string{"k8s-app": "metrics-server"}, 1))
		case "kube-prometheus-stack":
			out = append(out,
				svc(monitoringNS, "kube-prometheus-stack-prometheus", "ClusterIP", ip(),
					tcp(9090, "http-web"), map[string]string{"app.kubernetes.io/name": "prometheus"}, 1),
				svc(monitoringNS, "kube-prometheus-stack-grafana", "ClusterIP", ip(),
					tcp(80, "http-web"), map[string]string{"app.kubernetes.io/name": "grafana"}, 1),
				svc(monitoringNS, "kube-prometheus-stack-alertmanager", "ClusterIP", ip(),
					tcp(9093, "http-web"), map[string]string{"app.kubernetes.io/name": "alertmanager"}, 1),
			)
		case CertManagerAddon:
			out = append(out, svc("cert-manager", "cert-manager", "ClusterIP", ip(),
				[]ServicePort{{Name: "tcp-prometheus-servicemonitor", Protocol: "TCP", Port: 9402, TargetPort: "9402"}},
				map[string]string{"app.kubernetes.io/name": "cert-manager"}, 1))
		case ExternalDNSAddon:
			out = append(out, svc("external-dns", "external-dns", "ClusterIP", ip(),
				[]ServicePort{{Name: "http", Protocol: "TCP", Port: 7979, TargetPort: "http"}},
				map[string]string{"app.kubernetes.io/name": "external-dns"}, 1))
		case GatewayAddon:
			// The Envoy Gateway controller, and the Service the default Gateway's Envoy proxy fleet
			// publishes - the one MetalLB hands the cluster's reserved address to. Its name is the
			// shape Envoy Gateway mints (envoy-<ns>-<gateway>-<hash>), which is worth showing because
			// it is what an operator sees in `kubectl get svc` and would otherwise be a mystery.
			out = append(out, svc(DefaultGatewayNamespace, "envoy-gateway", "ClusterIP", ip(),
				[]ServicePort{{Name: "grpc", Protocol: "TCP", Port: 18000, TargetPort: "18000"}},
				map[string]string{"control-plane": "envoy-gateway"}, 1))
			if c.GatewayWired {
				proxy := svc(DefaultGatewayNamespace,
					"envoy-"+DefaultGatewayNamespace+"-"+DefaultGatewayName+"-"+randSuffix(c.ID, 5),
					"LoadBalancer", ip(),
					[]ServicePort{{Name: "http-80", Protocol: "TCP", Port: 80, TargetPort: "10080", NodePort: 31080}},
					map[string]string{"gateway.envoyproxy.io/owning-gateway-name": DefaultGatewayName}, 2)
				if f.tlsEnabled(c) {
					proxy.Ports = append(proxy.Ports, ServicePort{
						Name: "https-443", Protocol: "TCP", Port: 443, TargetPort: "10443", NodePort: 31443,
					})
				}
				if c.LoadBalancerIP != "" {
					proxy.ExternalIPs = []string{c.LoadBalancerIP}
				}
				out = append(out, proxy)
			}
		case MetalLBAddon:
			out = append(out, svc("metallb-system", "metallb-webhook-service", "ClusterIP", ip(),
				[]ServicePort{{Name: "https", Protocol: "TCP", Port: 443, TargetPort: "9443"}},
				map[string]string{"app.kubernetes.io/component": "controller"}, 1))
		}
	}

	// The demo application Services, matching the workload fake's "demo" namespace deployments.
	out = append(out,
		svc("demo", "web", "ClusterIP", ip(), tcp(80, "8080"), map[string]string{"app": "web"}, 3),
		svc("demo", "cache", "ClusterIP", "None",
			[]ServicePort{{Name: "redis", Protocol: "TCP", Port: 6379, TargetPort: "6379"}},
			map[string]string{"app": "cache"}, 2),
		svc("demo", "api", "ClusterIP", ip(), tcp(8080, "8080"), map[string]string{"app": "api"}, 2),
	)

	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// clusterIPFn hands out sequential, stable ClusterIPs from the service CIDR so the fake's addresses
// look like a real cluster's without any two Services colliding.
func clusterIPFn(start int) func() string {
	n := start
	return func() string {
		ip := fmt.Sprintf("10.96.%d.%d", n/250, n%250)
		n += 7
		return ip
	}
}

func controlPlaneNodes(c *domain.Cluster) []domain.Node {
	var cps []domain.Node
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane {
			cps = append(cps, n)
		}
	}
	if len(cps) == 0 && len(c.Nodes) > 0 {
		return c.Nodes[:1]
	}
	return cps
}

func (f *Fake) findService(c *domain.Cluster, ref ObjectRef) (ServiceSummary, bool) {
	for _, s := range f.buildServices(c) {
		if s.Namespace == ref.Namespace && s.Name == ref.Name {
			return s, true
		}
	}
	return ServiceSummary{}, false
}

// serviceBackends synthesizes the pod endpoints behind a Service, spread across the cluster's nodes.
func (f *Fake) serviceBackends(c *domain.Cluster, s ServiceSummary) []ServiceBackend {
	if s.Endpoints <= 0 {
		return []ServiceBackend{}
	}
	nodes := c.Nodes
	bs := make([]ServiceBackend, 0, s.Endpoints)
	for i := 0; i < s.Endpoints; i++ {
		b := ServiceBackend{
			Pod: fmt.Sprintf("%s-%s-%s", s.Name, randSuffix(s.Name, 7), randSuffix(s.Name, i)),
			IP:  fmt.Sprintf("10.244.%d.%d", i, 10+i*3),
		}
		if len(nodes) > 0 {
			b.Node = nodes[i%len(nodes)].VMName
		}
		bs = append(bs, b)
	}
	return bs
}

// tlsEnabled mirrors the default_gateway role's condition for the HTTPS listener: cert-manager on the
// cluster AND an apps domain to issue a wildcard for.
func (f *Fake) tlsEnabled(c *domain.Cluster) bool {
	return addonInstalled(c, CertManagerAddon) && c.AppsDomain != ""
}

// buildGateways returns the cluster's Gateways: the platform's default one once the reconciler has
// wired it, and nothing else - a user's own Gateways are theirs to create, and the fake inventing one
// would misrepresent what the platform ships.
func (f *Fake) buildGateways(c *domain.Cluster) []GatewaySummary {
	if !addonInstalled(c, GatewayAddon) || !c.GatewayWired {
		return []GatewaySummary{}
	}
	born := f.clusterEpoch(c)
	attached := len(f.buildRoutes(c))
	g := GatewaySummary{
		Namespace: DefaultGatewayNamespace,
		Name:      DefaultGatewayName,
		Class:     DefaultGatewayClass,
		Listeners: []GatewayListener{{
			Name: "http", Protocol: "HTTP", Port: 80,
			AttachedRoutes: attached, Programmed: true,
		}},
		Programmed: true,
		IsDefault:  true,
		CreatedAt:  born,
	}
	if c.LoadBalancerIP != "" {
		g.Addresses = []string{c.LoadBalancerIP}
	}
	if f.tlsEnabled(c) {
		g.Listeners = append(g.Listeners, GatewayListener{
			Name: "https", Protocol: "HTTPS", Port: 443,
			Hostname: "*." + c.AppsDomain, TLSMode: "Terminate",
			CertificateRefs: []string{"kaas-default-tls"},
			AttachedRoutes:  attached, Programmed: true,
		})
	}
	return []GatewaySummary{g}
}

// buildRoutes returns the synthesized HTTPRoutes: two demo apps published under the cluster's own
// apps domain, so the Overview's exposed-apps table shows the platform contract working end to end.
// Without an apps domain (a deployment with no KAAS_DNS_BASE_DOMAIN) the routes still exist but carry
// no hostname, which is exactly what a user gets in that configuration.
func (f *Fake) buildRoutes(c *domain.Cluster) []RouteSummary {
	if !addonInstalled(c, GatewayAddon) || !c.GatewayWired {
		return []RouteSummary{}
	}
	born := f.clusterEpoch(c)
	parent := []ParentRef{{
		Namespace: DefaultGatewayNamespace, Name: DefaultGatewayName, Accepted: true,
	}}
	host := func(sub string) []string {
		if c.AppsDomain == "" {
			return nil
		}
		return []string{sub + "." + c.AppsDomain}
	}
	return []RouteSummary{
		{
			Kind: KindHTTPRoute, Namespace: "demo", Name: "web",
			Hostnames: host("web"), ParentRefs: parent, Accepted: true, CreatedAt: born,
			Rules: []RouteRule{{
				Matches:  []string{"PathPrefix: /"},
				Backends: []RouteBackend{{Namespace: "demo", Name: "web", Kind: "Service", Port: 80, Weight: 1}},
			}},
		},
		{
			Kind: KindHTTPRoute, Namespace: "demo", Name: "api",
			Hostnames: host("api"), ParentRefs: parent, Accepted: true, CreatedAt: born,
			Rules: []RouteRule{{
				Matches:  []string{"PathPrefix: /v1"},
				Backends: []RouteBackend{{Namespace: "demo", Name: "api", Kind: "Service", Port: 8080, Weight: 1}},
			}},
		},
	}
}

// ---- synthesized YAML ----------------------------------------------------------

func (f *Fake) serviceManifest(s ServiceSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Service\nmetadata:\n  name: %s\n  namespace: %s\n", s.Name, s.Namespace)
	fmt.Fprintf(&b, "  creationTimestamp: \"%s\"\nspec:\n  type: %s\n", s.CreatedAt.UTC().Format(time.RFC3339), s.Type)
	if s.ClusterIP != "" {
		fmt.Fprintf(&b, "  clusterIP: %s\n", s.ClusterIP)
	}
	if len(s.Selector) > 0 {
		b.WriteString("  selector:\n")
		for _, k := range sortedKeys(s.Selector) {
			fmt.Fprintf(&b, "    %s: %s\n", k, s.Selector[k])
		}
	}
	b.WriteString("  ports:\n")
	for _, p := range s.Ports {
		fmt.Fprintf(&b, "    - name: %s\n      protocol: %s\n      port: %d\n      targetPort: %s\n", p.Name, p.Protocol, p.Port, p.TargetPort)
		if p.NodePort != 0 {
			fmt.Fprintf(&b, "      nodePort: %d\n", p.NodePort)
		}
	}
	if len(s.ExternalIPs) > 0 {
		b.WriteString("status:\n  loadBalancer:\n    ingress:\n")
		for _, ip := range s.ExternalIPs {
			fmt.Fprintf(&b, "      - ip: %s\n", ip)
		}
	}
	return b.String()
}

func (f *Fake) gatewayManifest(g GatewaySummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: gateway.networking.k8s.io/v1\nkind: Gateway\nmetadata:\n  name: %s\n  namespace: %s\n", g.Name, g.Namespace)
	fmt.Fprintf(&b, "spec:\n  gatewayClassName: %s\n", g.Class)
	if len(g.Addresses) > 0 {
		b.WriteString("  addresses:\n")
		for _, a := range g.Addresses {
			fmt.Fprintf(&b, "    - type: IPAddress\n      value: %s\n", a)
		}
	}
	b.WriteString("  listeners:\n")
	for _, l := range g.Listeners {
		fmt.Fprintf(&b, "    - name: %s\n      protocol: %s\n      port: %d\n", l.Name, l.Protocol, l.Port)
		if l.Hostname != "" {
			fmt.Fprintf(&b, "      hostname: \"%s\"\n", l.Hostname)
		}
		if l.TLSMode != "" {
			fmt.Fprintf(&b, "      tls:\n        mode: %s\n        certificateRefs:\n", l.TLSMode)
			for _, r := range l.CertificateRefs {
				fmt.Fprintf(&b, "          - kind: Secret\n            name: %s\n", r)
			}
		}
		b.WriteString("      allowedRoutes:\n        namespaces:\n          from: All\n")
	}
	return b.String()
}

func (f *Fake) routeManifest(r RouteSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: gateway.networking.k8s.io/v1\nkind: %s\nmetadata:\n  name: %s\n  namespace: %s\n",
		routeAPIKind(r.Kind), r.Name, r.Namespace)
	b.WriteString("spec:\n  parentRefs:\n")
	for _, p := range r.ParentRefs {
		fmt.Fprintf(&b, "    - name: %s\n      namespace: %s\n", p.Name, p.Namespace)
	}
	if len(r.Hostnames) > 0 {
		b.WriteString("  hostnames:\n")
		for _, h := range r.Hostnames {
			fmt.Fprintf(&b, "    - \"%s\"\n", h)
		}
	}
	b.WriteString("  rules:\n")
	for _, rule := range r.Rules {
		b.WriteString("    - matches:\n")
		for _, m := range rule.Matches {
			if path, ok := strings.CutPrefix(m, "PathPrefix: "); ok {
				fmt.Fprintf(&b, "        - path:\n            type: PathPrefix\n            value: %s\n", path)
			}
		}
		b.WriteString("      backendRefs:\n")
		for _, be := range rule.Backends {
			fmt.Fprintf(&b, "        - name: %s\n          port: %d\n", be.Name, be.Port)
		}
	}
	return b.String()
}

// routeAPIKind maps a wire RouteKind back to its Kubernetes Kind for the synthesized YAML.
func routeAPIKind(k RouteKind) string {
	switch k {
	case KindHTTPRoute:
		return "HTTPRoute"
	case KindGRPCRoute:
		return "GRPCRoute"
	case KindTCPRoute:
		return "TCPRoute"
	case KindTLSRoute:
		return "TLSRoute"
	case KindUDPRoute:
		return "UDPRoute"
	default:
		return string(k)
	}
}
