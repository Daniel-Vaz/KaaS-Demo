package audit

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Fake is the in-process Querier used in fake mode (make up-fake): it synthesizes a plausible,
// live-drifting stream of API-server audit events from a cluster's identity, so the whole Audit tab
// renders with no real cluster. Like the other query-seam fakes it is deterministic in the cluster id
// (and a real-time sequence number), so successive polls show a stable feed with genuinely new events
// arriving at the top - a live tail - rather than a random reshuffle.
type Fake struct{}

// NewFake returns the fake audit querier.
func NewFake() *Fake { return &Fake{} }

// fakeInterval is the synthetic gap between consecutive audit events. With ~one event per interval the
// generated window (fakeCount events) spans fakeCount*fakeInterval of history.
const (
	fakeInterval = 12 * time.Second
	fakeCount    = 240
)

// fakeEpoch anchors the synthetic event sequence to wall-clock time so the newest event's timestamp is
// always ~now and a poll fakeInterval later reveals one genuinely new event. Fixed reference point.
var fakeEpoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func (Fake) Events(_ context.Context, c *domain.Cluster, _ []byte, q Query) (*Page, error) {
	// seq is a monotonically-advancing counter driven by real time: the newest event's sequence
	// number. Event p positions back has sequence seq-p, timestamp epoch+seq*interval - so the whole
	// feed slides forward one slot every fakeInterval, exactly like tailing a live audit log.
	seq := int64(time.Since(fakeEpoch) / fakeInterval)
	events := make([]Event, 0, fakeCount)
	for p := 0; p < fakeCount; p++ {
		s := seq - int64(p)
		if s < 0 {
			break
		}
		events = append(events, fakeEvent(c, s))
	}
	return Assemble(events, q), nil
}

// actor is a canned identity the fake attributes events to, with the user-agent it presents.
type actor struct {
	user, agent string
	groups      []string
}

// resourceTemplate is a canned kind the fake generates events against. rbac marks the RBAC objects the
// real policy elevates to RequestResponse; namespaces is the pool an event's namespace is drawn from
// ("" = cluster-scoped).
type resourceTemplate struct {
	group, typ  string
	namespaces  []string
	rbac        bool
	clusterOnly bool
}

var fakeActors = []actor{
	{"kubernetes-admin", "kubectl/v1.36.2 (linux/amd64)", []string{"system:masters", "system:authenticated"}},
	{"system:serviceaccount:kube-system:deployment-controller", "kube-controller-manager/v1.36.2", []string{"system:serviceaccounts", "system:authenticated"}},
	{"system:serviceaccount:kube-system:cronjob-controller", "kube-controller-manager/v1.36.2", []string{"system:serviceaccounts", "system:authenticated"}},
	{"system:serviceaccount:monitoring-system:kube-prometheus-stack-operator", "prometheus-operator/v0.79.0", []string{"system:serviceaccounts", "system:authenticated"}},
	{"system:serviceaccount:trivy-system:trivy-operator", "trivy-operator/v0.24.0", []string{"system:serviceaccounts", "system:authenticated"}},
	{"alice@corp.example", "kubectl/v1.36.2 (darwin/arm64)", []string{"kaas:writers", "system:authenticated"}},
	{"bob@corp.example", "helm/v3.16.2", []string{"kaas:writers", "system:authenticated"}},
}

var fakeResources = []resourceTemplate{
	{"apps", "deployments", []string{"default", "payments", "storefront"}, false, false},
	{"apps", "statefulsets", []string{"payments", "monitoring-system"}, false, false},
	{"apps", "daemonsets", []string{"kube-system"}, false, false},
	{"", "configmaps", []string{"default", "kube-system", "payments"}, false, false},
	{"", "secrets", []string{"default", "payments", "trivy-system"}, false, false},
	{"", "services", []string{"default", "storefront"}, false, false},
	{"", "serviceaccounts", []string{"default", "payments"}, false, false},
	{"", "persistentvolumeclaims", []string{"payments", "monitoring-system"}, false, false},
	{"batch", "jobs", []string{"default", "payments"}, false, false},
	{"rbac.authorization.k8s.io", "rolebindings", []string{"default", "payments"}, true, false},
	{"rbac.authorization.k8s.io", "clusterroles", nil, true, true},
	{"", "nodes", nil, false, true},
}

// mutatingVerbs is the pool the fake draws from - biased to mutations because the default audit policy
// drops pure reads (get/list/watch), so the real feed is a "who changed what" stream too.
var mutatingVerbs = []string{"create", "update", "patch", "delete", "create", "update", "patch"}

// fakeEvent synthesizes the event with sequence number s for cluster c. Deterministic in (c.ID, s).
func fakeEvent(c *domain.Cluster, s int64) Event {
	base := fmt.Sprintf("%s|%d", c.ID, s)
	act := fakeActors[int(hash(base+"|actor"))%len(fakeActors)]
	res := fakeResources[int(hash(base+"|res"))%len(fakeResources)]
	verb := mutatingVerbs[int(hash(base+"|verb"))%len(mutatingVerbs)]

	ns := ""
	if !res.clusterOnly && len(res.namespaces) > 0 {
		ns = res.namespaces[int(hash(base+"|ns"))%len(res.namespaces)]
	}
	name := fakeName(res.typ, base)
	level := "Metadata"
	if res.rbac {
		level = "RequestResponse"
	}

	// Mostly-successful, with a realistic minority of denials (a viewer's forbidden write, an
	// optimistic-lock conflict, a stale delete).
	code := successCode(verb)
	switch hash(base+"|code") % 16 {
	case 0:
		code = 403
	case 1:
		code = 409
	case 2:
		code = 404
	}

	return Event{
		AuditID:      fakeAuditID(base),
		Timestamp:    fakeEpoch.Add(time.Duration(s) * fakeInterval).UTC().Format(time.RFC3339),
		Stage:        "ResponseComplete",
		Level:        level,
		Verb:         verb,
		User:         act.user,
		Groups:       act.groups,
		SourceIPs:    []string{fakeSourceIP(c, act.user)},
		UserAgent:    act.agent,
		Resource:     Resource{APIGroup: res.group, Type: res.typ, Namespace: ns, Name: name},
		RequestURI:   fakeRequestURI(res, ns, name),
		ResponseCode: code,
	}
}

func successCode(verb string) int {
	if verb == "create" {
		return 201
	}
	return 200
}

func fakeName(typ, seed string) string {
	adjectives := []string{"nginx", "billing", "checkout", "ledger", "cache", "storefront", "grafana", "regcred", "api", "worker"}
	a := adjectives[int(hash(seed+"|adj"))%len(adjectives)]
	switch typ {
	case "nodes":
		return fmt.Sprintf("node-%s", hex4(seed))
	case "clusterroles":
		return fmt.Sprintf("%s-role", a)
	case "secrets", "configmaps", "serviceaccounts":
		return a
	default:
		return fmt.Sprintf("%s-%s", a, hex4(seed))
	}
}

func fakeRequestURI(res resourceTemplate, ns, name string) string {
	prefix := "/api/v1"
	if res.group != "" {
		prefix = "/apis/" + res.group + "/v1"
	}
	if res.clusterOnly || ns == "" {
		return fmt.Sprintf("%s/%s/%s", prefix, res.typ, name)
	}
	return fmt.Sprintf("%s/namespaces/%s/%s/%s", prefix, ns, res.typ, name)
}

func fakeSourceIP(c *domain.Cluster, user string) string {
	// Controllers appear to come from a control-plane node; humans from an off-cluster admin subnet.
	if len(user) > 7 && user[:7] == "system:" {
		return fmt.Sprintf("10.%d.%d.%d", hash(c.ID)%64, hash(c.ID+"b")%256, 10+hash(c.ID+"c")%40)
	}
	return fmt.Sprintf("192.168.%d.%d", hash(user)%256, 2+hash(user+"h")%250)
}

func fakeAuditID(seed string) string {
	h := hash64(seed)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(h), uint16(h>>8), uint16(h>>16), uint16(h>>24), h&0xffffffffffff)
}

func hash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func hex4(s string) string { return fmt.Sprintf("%04x", hash(s)&0xffff) }
