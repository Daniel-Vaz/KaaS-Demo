package kubectl

import (
	"context"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
)

// Real API-server shapes for the Networking page's reads. The interesting cases are the ones the UI
// would get wrong on its own: a LoadBalancer whose address lives in status (not spec), a headless
// Service with clusterIP "None", a targetPort that is a NAME rather than a number, and a Gateway
// whose per-listener status lives in a parallel array keyed by listener name.

const serviceList = `{"items":[
  {"metadata":{"name":"kubernetes","namespace":"default","creationTimestamp":"2026-01-01T00:00:00Z"},
   "spec":{"type":"ClusterIP","clusterIP":"10.96.0.1",
           "ports":[{"name":"https","protocol":"TCP","port":443,"targetPort":6443}]}},
  {"metadata":{"name":"cache","namespace":"demo"},
   "spec":{"type":"ClusterIP","clusterIP":"None","selector":{"app":"cache"},
           "ports":[{"name":"redis","protocol":"TCP","port":6379,"targetPort":"redis"}]}},
  {"metadata":{"name":"envoy-eg","namespace":"envoy-gateway-system"},
   "spec":{"type":"LoadBalancer","clusterIP":"10.96.1.7",
           "ports":[{"name":"http","protocol":"TCP","port":80,"targetPort":10080,"nodePort":31080}]},
   "status":{"loadBalancer":{"ingress":[{"ip":"10.10.0.9"}]}}}
]}`

const endpointsList = `{"items":[
  {"metadata":{"name":"cache","namespace":"demo"},
   "subsets":[{"addresses":[{"ip":"10.244.1.5","nodeName":"dev-default-0","targetRef":{"kind":"Pod","name":"cache-0"}},
                            {"ip":"10.244.1.6","nodeName":"dev-default-1","targetRef":{"kind":"Pod","name":"cache-1"}}]}]},
  {"metadata":{"name":"envoy-eg","namespace":"envoy-gateway-system"},"subsets":[]}
]}`

func TestServicesParsing(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{
		"get services":  serviceList,
		"get endpoints": endpointsList,
	}})
	ss, err := c.Services(context.Background(), cl, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 3 {
		t.Fatalf("got %d services, want 3", len(ss))
	}

	// A numeric targetPort renders as a number; a named one keeps its name (the IntOrString union).
	if ss[0].Ports[0].TargetPort != "6443" {
		t.Errorf("kubernetes targetPort = %q, want 6443", ss[0].Ports[0].TargetPort)
	}
	if ss[1].Ports[0].TargetPort != "redis" {
		t.Errorf("cache targetPort = %q, want the port NAME", ss[1].Ports[0].TargetPort)
	}
	// A headless Service keeps "None" rather than being blanked - it is meaningful.
	if ss[1].ClusterIP != "None" {
		t.Errorf("cache clusterIP = %q, want None", ss[1].ClusterIP)
	}
	if ss[1].Endpoints != 2 {
		t.Errorf("cache endpoints = %d, want 2", ss[1].Endpoints)
	}

	// The LoadBalancer's external address comes from STATUS - the assignment MetalLB made, which is
	// the address the whole page is about.
	lb := ss[2]
	if len(lb.ExternalIPs) != 1 || lb.ExternalIPs[0] != "10.10.0.9" {
		t.Errorf("envoy-eg external = %v, want [10.10.0.9]", lb.ExternalIPs)
	}
	if lb.Ports[0].NodePort != 31080 {
		t.Errorf("envoy-eg nodePort = %d, want 31080", lb.Ports[0].NodePort)
	}
	if lb.Endpoints != 0 {
		t.Errorf("envoy-eg endpoints = %d, want 0 (empty subsets)", lb.Endpoints)
	}
}

const gatewayList = `{"items":[
  {"metadata":{"name":"eg","namespace":"envoy-gateway-system","creationTimestamp":"2026-01-01T00:00:00Z"},
   "spec":{"gatewayClassName":"eg","addresses":[{"type":"IPAddress","value":"10.10.0.9"}],
           "listeners":[
             {"name":"http","protocol":"HTTP","port":80},
             {"name":"https","protocol":"HTTPS","port":443,"hostname":"*.apps.dev.example",
              "tls":{"mode":"Terminate","certificateRefs":[{"name":"kaas-default-tls"}]}}]},
   "status":{"addresses":[{"type":"IPAddress","value":"10.10.0.9"}],
             "conditions":[{"type":"Accepted","status":"True"},{"type":"Programmed","status":"True"}],
             "listeners":[
               {"name":"http","attachedRoutes":2,"conditions":[{"type":"Programmed","status":"True"}]},
               {"name":"https","attachedRoutes":2,
                "conditions":[{"type":"Programmed","status":"False","reason":"Invalid","message":"secret not found"}]}]}}
]}`

func TestGatewaysParsing(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{"get gateways": gatewayList}})
	gs, err := c.Gateways(context.Background(), cl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 {
		t.Fatalf("got %d gateways, want 1", len(gs))
	}
	g := gs[0]
	if !g.IsDefault {
		t.Error("the platform's own gateway is not marked default")
	}
	if !g.Programmed {
		t.Error("Programmed = false on a Programmed=True gateway")
	}
	if len(g.Listeners) != 2 {
		t.Fatalf("got %d listeners, want 2", len(g.Listeners))
	}
	// Per-listener status is a parallel array keyed by name, so it must land on the right listener.
	if !g.Listeners[0].Programmed || g.Listeners[0].AttachedRoutes != 2 {
		t.Errorf("http listener = %+v, want programmed with 2 routes", g.Listeners[0])
	}
	https := g.Listeners[1]
	if https.Programmed {
		t.Error("https listener reported programmed despite Programmed=False")
	}
	if !strings.Contains(https.Status, "secret not found") {
		t.Errorf("https listener status = %q, want the controller's message", https.Status)
	}
	if https.TLSMode != "Terminate" || len(https.CertificateRefs) != 1 {
		t.Errorf("https listener TLS = %q %v", https.TLSMode, https.CertificateRefs)
	}
}

const httpRouteList = `{"items":[
  {"metadata":{"name":"web","namespace":"demo","creationTimestamp":"2026-01-01T00:00:00Z"},
   "spec":{"hostnames":["web.apps.dev.example"],
           "parentRefs":[{"name":"eg","namespace":"envoy-gateway-system"}],
           "rules":[{"matches":[{"path":{"type":"PathPrefix","value":"/"}}],
                     "backendRefs":[{"name":"web","port":80,"weight":1}]}]},
   "status":{"parents":[{"parentRef":{"name":"eg","namespace":"envoy-gateway-system"},
                         "conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}},
  {"metadata":{"name":"stale","namespace":"demo"},
   "spec":{"hostnames":["nope.example.com"],
           "parentRefs":[{"name":"eg"}],
           "rules":[{"backendRefs":[{"name":"api","port":8080}]}]},
   "status":{"parents":[{"parentRef":{"name":"eg","namespace":"demo"},
                         "conditions":[{"type":"Accepted","status":"False","reason":"NoMatchingListenerHostname"}]}]}}
]}`

func TestRoutesParsing(t *testing.T) {
	// Only httproutes exists here - the realistic case, since Envoy Gateway installs the whole
	// Gateway API but a cluster only ever has HTTPRoutes in it. The other four kinds answer as
	// missing, which must not fail the read.
	c := New(routeExecer{})
	rs, err := c.Routes(context.Background(), cl, nil, "")
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d routes, want 2", len(rs))
	}

	// Routes sort by namespace then name, so "stale" precedes "web".
	stale, web := rs[0], rs[1]
	if web.Name != "web" || stale.Name != "stale" {
		t.Fatalf("unexpected order: %q, %q", stale.Name, web.Name)
	}
	if !web.Accepted {
		t.Error("web route not accepted despite Accepted=True")
	}
	if web.ParentRefs[0].Namespace != "envoy-gateway-system" {
		t.Errorf("web parent namespace = %q", web.ParentRefs[0].Namespace)
	}
	if len(web.Rules) != 1 || web.Rules[0].Matches[0] != "PathPrefix: /" {
		t.Errorf("web rules = %+v", web.Rules)
	}
	if b := web.Rules[0].Backends[0]; b.Name != "web" || b.Port != 80 || b.Namespace != "demo" {
		t.Errorf("web backend = %+v, want demo/web:80", b)
	}

	// A parentRef with no namespace defaults to the route's own - and the status entry keyed the
	// same way must still match it, so the refusal reason reaches the UI.
	if stale.Accepted {
		t.Error("stale route reported accepted")
	}
	if stale.ParentRefs[0].Namespace != "demo" {
		t.Errorf("stale parent namespace = %q, want the route's own", stale.ParentRefs[0].Namespace)
	}
	if !strings.Contains(stale.Status, "NoMatchingListenerHostname") {
		t.Errorf("stale status = %q, want the refusal reason", stale.Status)
	}
}

// A cluster with no Gateway API at all (envoy-gateway deselected) must read as empty, not as an
// error - otherwise the Services tab and the platform overview break along with it.
func TestGatewayAPIAbsentIsNotAnError(t *testing.T) {
	c := New(missingResourceExecer{})

	gs, err := c.Gateways(context.Background(), cl, nil)
	if err != nil {
		t.Fatalf("Gateways with no CRD: %v", err)
	}
	if len(gs) != 0 {
		t.Errorf("got %d gateways, want none", len(gs))
	}
	rs, err := c.Routes(context.Background(), cl, nil, "")
	if err != nil {
		t.Fatalf("Routes with no CRD: %v", err)
	}
	if len(rs) != 0 {
		t.Errorf("got %d routes, want none", len(rs))
	}
}

// routeExecer serves HTTPRoutes and reports every other route kind as a type the cluster doesn't
// have - the shape a real cluster has, since only HTTPRoutes are in use.
type routeExecer struct{}

func (routeExecer) Run(ctx context.Context, kc []byte, id string, args []string) (Result, error) {
	if strings.Contains(strings.Join(args, " "), "get httproutes") {
		return Result{Stdout: []byte(httpRouteList)}, nil
	}
	return missingResourceExecer{}.Run(ctx, kc, id, args)
}

func (routeExecer) Stream(context.Context, []byte, string, []string, kube.LogSink) error {
	return nil
}

// missingResourceExecer answers every call the way kubectl does for an unknown type.
type missingResourceExecer struct{}

func (missingResourceExecer) Run(_ context.Context, _ []byte, _ string, args []string) (Result, error) {
	return Result{
		Stderr: `error: the server doesn't have a resource type "` + args[1] + `"`,
		Code:   1,
	}, nil
}

func (missingResourceExecer) Stream(context.Context, []byte, string, []string, kube.LogSink) error {
	return nil
}
