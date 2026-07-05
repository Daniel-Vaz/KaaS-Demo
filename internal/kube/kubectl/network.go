package kubectl

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
)

// The kube.NetworkReader half of the real Client: Services and Gateway API Gateways/Routes read with
// the same Execer, arg-building and JSON shapes as the workload and storage halves (see kubectl.go).
//
// The one thing this half does that the others don't is tolerate a MISSING resource type. The Gateway
// API CRDs come from the envoy-gateway add-on, which a user may deselect, and `kubectl get` on an
// unknown type is a hard error. Every Gateway API read therefore goes through optional(), which turns
// "no such resource type" into an empty result - so the Services tab and the Overview's platform half
// keep working on a cluster with no Gateway API at all.

func (c *Client) Services(ctx context.Context, cl *domain.Cluster, kc []byte, namespace string) ([]kube.ServiceSummary, error) {
	svcs, err := c.services(ctx, cl, kc, namespace)
	if err != nil {
		return nil, err
	}
	// Endpoint counts come from ONE list call indexed by ns/name rather than a get per Service: the
	// list is the common case and a per-row round trip would make it O(services) exec hops.
	counts := c.endpointCounts(ctx, cl, kc, namespace)
	res := make([]kube.ServiceSummary, 0, len(svcs))
	for _, s := range svcs {
		sum := s.summary()
		sum.Endpoints = counts[s.Metadata.Namespace+"/"+s.Metadata.Name]
		res = append(res, sum)
	}
	return res, nil
}

func (c *Client) Service(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.ObjectRef) (*kube.ServiceDetail, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "services", ref.Name, "-n", ref.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var obj rawService
	if err := json.Unmarshal(out, &obj); err != nil {
		return nil, fmt.Errorf("decode service: %w", err)
	}
	detail := obj.detail()
	// Backends are best-effort enrichment, like a claim's mounting pods: a Service with no endpoints
	// (or an endpoints read we're not permitted) is exactly when an operator wants to see the rest.
	if eps, eerr := c.endpoints(ctx, cl, kc, ref); eerr == nil {
		detail.Backends = eps
		detail.Endpoints = len(eps)
	}
	return &detail, nil
}

func (c *Client) ServiceManifest(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.ObjectRef) (string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "services", ref.Name, "-n", ref.Namespace, "-o", "yaml")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *Client) ServiceEvents(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.ObjectRef) ([]kube.Event, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "events", "-n", ref.Namespace,
		"--field-selector", "involvedObject.kind=Service,involvedObject.name="+ref.Name,
		"-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawEvent `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode events: %w", err)
	}
	res := make([]kube.Event, 0, len(list.Items))
	for _, e := range list.Items {
		res = append(res, e.event())
	}
	sort.Slice(res, func(i, j int) bool { return res[i].LastSeen.After(res[j].LastSeen) })
	if len(res) > 100 {
		res = res[:100]
	}
	return res, nil
}

func (c *Client) Gateways(ctx context.Context, cl *domain.Cluster, kc []byte) ([]kube.GatewaySummary, error) {
	out, missing, err := c.optional(ctx, kc, cl.ID, "get", "gateways.gateway.networking.k8s.io", "--all-namespaces", "-o", "json")
	if err != nil || missing {
		return []kube.GatewaySummary{}, err
	}
	var list struct {
		Items []rawGateway `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode gateways: %w", err)
	}
	res := make([]kube.GatewaySummary, 0, len(list.Items))
	for _, it := range list.Items {
		res = append(res, it.summary())
	}
	sort.Slice(res, func(i, j int) bool {
		// The platform's own Gateway first - it is the one the cluster's contract is written against.
		if res[i].IsDefault != res[j].IsDefault {
			return res[i].IsDefault
		}
		if res[i].Namespace != res[j].Namespace {
			return res[i].Namespace < res[j].Namespace
		}
		return res[i].Name < res[j].Name
	})
	return res, nil
}

func (c *Client) GatewayManifest(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.ObjectRef) (string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "gateways.gateway.networking.k8s.io", ref.Name, "-n", ref.Namespace, "-o", "yaml")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *Client) Routes(ctx context.Context, cl *domain.Cluster, kc []byte, namespace string) ([]kube.RouteSummary, error) {
	var res []kube.RouteSummary
	for _, k := range kube.AllRouteKinds {
		rs, err := c.routesOfKind(ctx, cl, kc, k, namespace)
		if err != nil {
			return nil, err
		}
		res = append(res, rs...)
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Namespace != res[j].Namespace {
			return res[i].Namespace < res[j].Namespace
		}
		return res[i].Name < res[j].Name
	})
	return res, nil
}

func (c *Client) routesOfKind(ctx context.Context, cl *domain.Cluster, kc []byte, kind kube.RouteKind, namespace string) ([]kube.RouteSummary, error) {
	args := []string{"get", kind.Resource()}
	if namespace == "" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "-n", namespace)
	}
	args = append(args, "-o", "json")
	out, missing, err := c.optional(ctx, kc, cl.ID, args...)
	if err != nil || missing {
		return nil, err
	}
	var list struct {
		Items []rawRoute `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode %ss: %w", kind, err)
	}
	res := make([]kube.RouteSummary, 0, len(list.Items))
	for _, it := range list.Items {
		res = append(res, it.summary(kind))
	}
	return res, nil
}

func (c *Client) RouteManifest(ctx context.Context, cl *domain.Cluster, kc []byte, kind kube.RouteKind, ref kube.ObjectRef) (string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", kind.Resource(), ref.Name, "-n", ref.Namespace, "-o", "yaml")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *Client) NetworkOverview(ctx context.Context, cl *domain.Cluster, kc []byte) (*kube.NetworkOverview, error) {
	ov := kube.PlatformOverview(cl)

	// Services are the only read that must succeed: they are core objects, and a failure here means
	// the cluster isn't answering at all, which the page should report rather than paper over.
	svcs, err := c.Services(ctx, cl, kc, "")
	if err != nil {
		return nil, err
	}
	ov.ServiceCount = len(svcs)
	for _, s := range svcs {
		if s.Type == "LoadBalancer" || len(s.ExternalIPs) > 0 {
			ov.LoadBalancerServices = append(ov.LoadBalancerServices, s)
		}
	}

	gws, err := c.Gateways(ctx, cl, kc)
	if err != nil {
		return nil, err
	}
	ov.GatewayCount = len(gws)
	for i := range gws {
		if gws[i].IsDefault {
			g := gws[i]
			ov.DefaultGateway = &g
			break
		}
	}

	routes, err := c.Routes(ctx, cl, kc, "")
	if err != nil {
		return nil, err
	}
	ov.RouteCount = len(routes)
	if apps := kube.ExposedApps(routes, gws, cl.AppsDomain); len(apps) > 0 {
		ov.ExposedApps = apps
	}
	return &ov, nil
}

// ---- helpers -------------------------------------------------------------------

// optional runs a read that may target a resource type the cluster doesn't have (a Gateway API CRD
// on a cluster without the envoy-gateway add-on). It reports missing=true instead of an error in
// that case; every other failure is returned as usual.
func (c *Client) optional(ctx context.Context, kc []byte, id string, args ...string) (out []byte, missing bool, err error) {
	out, err = c.run(ctx, kc, id, args...)
	if err != nil && isMissingResource(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return out, false, nil
}

// isMissingResource matches kubectl's two ways of saying "this type doesn't exist here". Matching on
// the message is unlovely, but the exec seam gives us stderr and an exit code and nothing structured
// - and the alternative (an api-resources probe before every read) doubles the round trips.
func isMissingResource(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "the server doesn't have a resource type") ||
		strings.Contains(msg, "the server could not find the requested resource") ||
		strings.Contains(msg, "could not find the requested resource")
}

func (c *Client) services(ctx context.Context, cl *domain.Cluster, kc []byte, namespace string) ([]rawService, error) {
	args := []string{"get", "services"}
	if namespace == "" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "-n", namespace)
	}
	args = append(args, "-o", "json")
	out, err := c.run(ctx, kc, cl.ID, args...)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawService `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode services: %w", err)
	}
	return list.Items, nil
}

// endpointCounts returns ready-endpoint counts keyed "namespace/name". Best-effort: a failure yields
// an empty map, so the column reads 0 rather than failing the whole list.
func (c *Client) endpointCounts(ctx context.Context, cl *domain.Cluster, kc []byte, namespace string) map[string]int {
	args := []string{"get", "endpoints"}
	if namespace == "" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "-n", namespace)
	}
	args = append(args, "-o", "json")
	out, err := c.run(ctx, kc, cl.ID, args...)
	if err != nil {
		return map[string]int{}
	}
	var list struct {
		Items []rawEndpoints `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return map[string]int{}
	}
	counts := make(map[string]int, len(list.Items))
	for _, e := range list.Items {
		n := 0
		for _, s := range e.Subsets {
			n += len(s.Addresses)
		}
		counts[e.Metadata.Namespace+"/"+e.Metadata.Name] = n
	}
	return counts
}

// endpoints lists one Service's ready endpoints as backends.
func (c *Client) endpoints(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.ObjectRef) ([]kube.ServiceBackend, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "endpoints", ref.Name, "-n", ref.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var obj rawEndpoints
	if err := json.Unmarshal(out, &obj); err != nil {
		return nil, fmt.Errorf("decode endpoints: %w", err)
	}
	var bs []kube.ServiceBackend
	for _, s := range obj.Subsets {
		for _, a := range s.Addresses {
			b := kube.ServiceBackend{IP: a.IP, Node: a.NodeName}
			if a.TargetRef != nil && a.TargetRef.Kind == "Pod" {
				b.Pod = a.TargetRef.Name
			}
			bs = append(bs, b)
		}
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].IP < bs[j].IP })
	return bs, nil
}

// ---- kubectl -o json shapes ----------------------------------------------------

type rawService struct {
	Metadata storageMeta `json:"metadata"`
	Spec     struct {
		Type                  string            `json:"type"`
		ClusterIP             string            `json:"clusterIP"`
		ExternalIPs           []string          `json:"externalIPs"`
		ExternalName          string            `json:"externalName"`
		SessionAffinity       string            `json:"sessionAffinity"`
		ExternalTrafficPolicy string            `json:"externalTrafficPolicy"`
		IPFamilies            []string          `json:"ipFamilies"`
		Selector              map[string]string `json:"selector"`
		Ports                 []struct {
			Name        string          `json:"name"`
			Protocol    string          `json:"protocol"`
			AppProtocol string          `json:"appProtocol"`
			Port        int             `json:"port"`
			TargetPort  json.RawMessage `json:"targetPort"`
			NodePort    int             `json:"nodePort"`
		} `json:"ports"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

func (s rawService) summary() kube.ServiceSummary {
	typ := s.Spec.Type
	if typ == "" {
		typ = "ClusterIP" // the API server defaults it, but a hand-written object may omit it
	}
	ports := make([]kube.ServicePort, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		ports = append(ports, kube.ServicePort{
			Name:       p.Name,
			Protocol:   p.Protocol,
			Port:       p.Port,
			TargetPort: intOrString(p.TargetPort),
			NodePort:   p.NodePort,
			AppProto:   p.AppProtocol,
		})
	}
	// The assigned ingress addresses first (MetalLB's, on this platform), then any spec.externalIPs:
	// both are "reachable from outside", and the assigned one is the interesting one.
	var ext []string
	for _, in := range s.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			ext = append(ext, in.IP)
		} else if in.Hostname != "" {
			ext = append(ext, in.Hostname)
		}
	}
	ext = append(ext, s.Spec.ExternalIPs...)
	return kube.ServiceSummary{
		Namespace:   s.Metadata.Namespace,
		Name:        s.Metadata.Name,
		Type:        typ,
		ClusterIP:   s.Spec.ClusterIP,
		ExternalIPs: ext,
		Ports:       ports,
		Selector:    s.Spec.Selector,
		CreatedAt:   s.Metadata.CreationTimestamp,
	}
}

func (s rawService) detail() kube.ServiceDetail {
	return kube.ServiceDetail{
		ServiceSummary:        s.summary(),
		Labels:                s.Metadata.Labels,
		Annotations:           s.Metadata.Annotations,
		SessionAffinity:       s.Spec.SessionAffinity,
		ExternalTrafficPolicy: s.Spec.ExternalTrafficPolicy,
		IPFamilies:            s.Spec.IPFamilies,
		ExternalName:          s.Spec.ExternalName,
		Backends:              []kube.ServiceBackend{},
	}
}

type rawEndpoints struct {
	Metadata storageMeta `json:"metadata"`
	Subsets  []struct {
		Addresses []struct {
			IP        string `json:"ip"`
			NodeName  string `json:"nodeName"`
			TargetRef *struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"targetRef"`
		} `json:"addresses"`
	} `json:"subsets"`
}

type rawGateway struct {
	Metadata storageMeta `json:"metadata"`
	Spec     struct {
		GatewayClassName string `json:"gatewayClassName"`
		Addresses        []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"addresses"`
		Listeners []struct {
			Name     string `json:"name"`
			Protocol string `json:"protocol"`
			Port     int    `json:"port"`
			Hostname string `json:"hostname"`
			TLS      *struct {
				Mode            string `json:"mode"`
				CertificateRefs []struct {
					Name string `json:"name"`
				} `json:"certificateRefs"`
			} `json:"tls"`
		} `json:"listeners"`
	} `json:"spec"`
	Status struct {
		Addresses []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"addresses"`
		Conditions []gwCondition `json:"conditions"`
		Listeners  []struct {
			Name           string        `json:"name"`
			AttachedRoutes int           `json:"attachedRoutes"`
			Conditions     []gwCondition `json:"conditions"`
		} `json:"listeners"`
	} `json:"status"`
}

type gwCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (g rawGateway) summary() kube.GatewaySummary {
	// status.addresses is what the Gateway actually got (MetalLB's assignment); spec.addresses is
	// what was asked for. Prefer the former and fall back so a not-yet-programmed Gateway still shows
	// the address it is waiting on.
	var addrs []string
	for _, a := range g.Status.Addresses {
		addrs = append(addrs, a.Value)
	}
	if len(addrs) == 0 {
		for _, a := range g.Spec.Addresses {
			addrs = append(addrs, a.Value)
		}
	}

	// Per-listener status is keyed by listener name in a parallel array.
	type lstat struct {
		attached int
		conds    []gwCondition
	}
	stats := make(map[string]lstat, len(g.Status.Listeners))
	for _, l := range g.Status.Listeners {
		stats[l.Name] = lstat{attached: l.AttachedRoutes, conds: l.Conditions}
	}

	ls := make([]kube.GatewayListener, 0, len(g.Spec.Listeners))
	for _, l := range g.Spec.Listeners {
		gl := kube.GatewayListener{
			Name:     l.Name,
			Protocol: l.Protocol,
			Port:     l.Port,
			Hostname: l.Hostname,
		}
		if l.TLS != nil {
			gl.TLSMode = l.TLS.Mode
			if gl.TLSMode == "" {
				gl.TLSMode = "Terminate" // the API's default when tls is present
			}
			for _, r := range l.TLS.CertificateRefs {
				gl.CertificateRefs = append(gl.CertificateRefs, r.Name)
			}
		}
		st := stats[l.Name]
		gl.AttachedRoutes = st.attached
		gl.Programmed, gl.Status = conditionState(st.conds, "Programmed", "Accepted")
		ls = append(ls, gl)
	}

	programmed, status := conditionState(g.Status.Conditions, "Programmed", "Accepted")
	ref := kube.ObjectRef{Namespace: g.Metadata.Namespace, Name: g.Metadata.Name}
	return kube.GatewaySummary{
		Namespace:  ref.Namespace,
		Name:       ref.Name,
		Class:      g.Spec.GatewayClassName,
		Addresses:  addrs,
		Listeners:  ls,
		Programmed: programmed,
		Status:     status,
		IsDefault:  kube.IsDefaultGateway(ref),
		Labels:     g.Metadata.Labels,
		CreatedAt:  g.Metadata.CreationTimestamp,
	}
}

// rawRoute covers every Gateway API route kind: they share parentRefs/hostnames/status, and differ
// only in the match shape inside each rule - which is decoded loosely (matches is left generic) so
// one struct serves HTTPRoute, GRPCRoute and the L4 kinds alike.
type rawRoute struct {
	Metadata storageMeta `json:"metadata"`
	Spec     struct {
		Hostnames  []string     `json:"hostnames"`
		ParentRefs []rawParent  `json:"parentRefs"`
		Rules      []rawRuleObj `json:"rules"`
	} `json:"spec"`
	Status struct {
		Parents []struct {
			ParentRef  rawParent     `json:"parentRef"`
			Conditions []gwCondition `json:"conditions"`
		} `json:"parents"`
	} `json:"status"`
}

type rawParent struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	SectionName string `json:"sectionName"`
	Port        int    `json:"port"`
}

type rawRuleObj struct {
	Matches []struct {
		Path *struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"path"`
		Method  string `json:"method"`
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
		QueryParams []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"queryParams"`
		Service string `json:"service"` // GRPCRoute
	} `json:"matches"`
	BackendRefs []struct {
		Group     string `json:"group"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Port      int    `json:"port"`
		Weight    *int   `json:"weight"`
	} `json:"backendRefs"`
}

func (r rawRoute) summary(kind kube.RouteKind) kube.RouteSummary {
	ns := r.Metadata.Namespace

	// Index the status by parent so each spec parentRef can report whether it was accepted. The key
	// resolves the optional namespace the same way the spec side does, so the two always line up.
	type pstat struct {
		accepted bool
		status   string
	}
	stats := map[string]pstat{}
	for _, p := range r.Status.Parents {
		accepted, status := conditionState(p.Conditions, "Accepted", "ResolvedRefs")
		stats[parentKey(p.ParentRef, ns)] = pstat{accepted: accepted, status: status}
	}

	parents := make([]kube.ParentRef, 0, len(r.Spec.ParentRefs))
	anyAccepted := false
	routeStatus := ""
	for _, p := range r.Spec.ParentRefs {
		pns := p.Namespace
		if pns == "" {
			pns = ns // the Gateway API defaults a parentRef's namespace to the route's own
		}
		st := stats[parentKey(p, ns)]
		if st.accepted {
			anyAccepted = true
		} else if routeStatus == "" {
			routeStatus = st.status
		}
		parents = append(parents, kube.ParentRef{
			Namespace:   pns,
			Name:        p.Name,
			SectionName: p.SectionName,
			Port:        p.Port,
			Accepted:    st.accepted,
			Status:      st.status,
		})
	}

	rules := make([]kube.RouteRule, 0, len(r.Spec.Rules))
	for _, ru := range r.Spec.Rules {
		rule := kube.RouteRule{Backends: []kube.RouteBackend{}}
		for _, m := range ru.Matches {
			if m.Path != nil && m.Path.Value != "" {
				t := m.Path.Type
				if t == "" {
					t = "PathPrefix"
				}
				rule.Matches = append(rule.Matches, t+": "+m.Path.Value)
			}
			if m.Method != "" {
				rule.Matches = append(rule.Matches, "Method: "+m.Method)
			}
			if m.Service != "" {
				rule.Matches = append(rule.Matches, "Service: "+m.Service)
			}
			for _, h := range m.Headers {
				rule.Matches = append(rule.Matches, "Header "+h.Name+"="+h.Value)
			}
			for _, q := range m.QueryParams {
				rule.Matches = append(rule.Matches, "Query "+q.Name+"="+q.Value)
			}
		}
		for _, b := range ru.BackendRefs {
			k := b.Kind
			if k == "" {
				k = "Service"
			}
			bn := b.Namespace
			if bn == "" {
				bn = ns
			}
			rb := kube.RouteBackend{Namespace: bn, Name: b.Name, Kind: k, Port: b.Port}
			if b.Weight != nil {
				rb.Weight = *b.Weight
			}
			rule.Backends = append(rule.Backends, rb)
		}
		rules = append(rules, rule)
	}

	return kube.RouteSummary{
		Kind:       kind,
		Namespace:  ns,
		Name:       r.Metadata.Name,
		Hostnames:  r.Spec.Hostnames,
		ParentRefs: parents,
		Rules:      rules,
		Accepted:   anyAccepted,
		Status:     routeStatus,
		Labels:     r.Metadata.Labels,
		CreatedAt:  r.Metadata.CreationTimestamp,
	}
}

// ---- small helpers -------------------------------------------------------------

// parentKey identifies a parentRef for matching a spec entry to its status entry, defaulting the
// namespace to the route's own exactly as the Gateway API does.
func parentKey(p rawParent, routeNS string) string {
	ns := p.Namespace
	if ns == "" {
		ns = routeNS
	}
	return ns + "/" + p.Name + "/" + p.SectionName
}

// conditionState reports whether the primary condition is True, and otherwise a short reason drawn
// from the first non-True condition among those named - the message an operator needs ("no matching
// listener hostname", "address not assigned") without dumping the whole condition list into a table.
func conditionState(conds []gwCondition, primary string, also ...string) (bool, string) {
	want := append([]string{primary}, also...)
	ok := false
	reason := ""
	for _, c := range conds {
		for _, w := range want {
			if c.Type != w {
				continue
			}
			if c.Status == "True" {
				if w == primary {
					ok = true
				}
				continue
			}
			if reason == "" {
				reason = c.Reason
				if c.Message != "" {
					reason = c.Reason + ": " + c.Message
				}
			}
		}
	}
	if ok {
		return true, ""
	}
	if reason == "" && len(conds) == 0 {
		reason = "Pending" // no status yet - the controller hasn't reconciled it
	}
	return false, reason
}

// intOrString renders a Kubernetes IntOrString (a Service's targetPort) as a plain string.
func intOrString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.Itoa(n)
	}
	return strings.Trim(string(raw), `"`)
}
