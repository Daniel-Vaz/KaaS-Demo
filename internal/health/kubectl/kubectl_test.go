package kubectl

import (
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func TestParseNodesAndReadyCheck(t *testing.T) {
	raw := []byte(`{"items":[
		{"metadata":{"name":"cp-0"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"w-0"},"spec":{"unschedulable":true},"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"True"}]}},
		{"metadata":{"name":"w-1"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}
	]}`)
	nodes, nh := parseNodes(raw, nil)
	if len(nodes) != 3 || len(nh) != 3 {
		t.Fatalf("parseNodes: got %d nodes / %d health", len(nodes), len(nh))
	}
	// w-0 is cordoned AND carries the pressure - cordon is independent of Ready (still Ready here);
	// w-1 is NotReady.
	if got := nh[1]; got.NodeName != "w-0" || !got.Ready || !got.Cordoned || len(got.Pressures) != 1 || got.Pressures[0] != "MemoryPressure" {
		t.Errorf("w-0 health = %+v", got)
	}
	if nh[2].Ready {
		t.Errorf("w-1 should be NotReady")
	}
	if nh[2].Cordoned {
		t.Errorf("w-1 should not be cordoned")
	}

	// Cordoning doesn't affect the Ready check - nodesReadyCheck only reacts to w-1's NotReady.
	chk := nodesReadyCheck(nodes, nil)
	if chk.Status != domain.HealthUnhealthy {
		t.Errorf("nodesReadyCheck status = %s, want unhealthy", chk.Status)
	}
	if !strings.Contains(chk.Summary, "w-1") {
		t.Errorf("summary should name NotReady node: %q", chk.Summary)
	}
	if strings.Contains(chk.Summary, "w-0") {
		t.Errorf("cordoned-but-Ready node should not be named as NotReady: %q", chk.Summary)
	}

	// A query error makes the check unknown, not a false-negative.
	if chk := nodesReadyCheck(nil, errBoom); chk.Status != domain.HealthUnknown {
		t.Errorf("errored nodesReadyCheck = %s, want unknown", chk.Status)
	}
}

func TestSystemWorkloadsCheck(t *testing.T) {
	// A crash-looping kube-system pod is unhealthy; pods outside kube-system are ignored.
	raw := []byte(`{"items":[
		{"metadata":{"name":"coredns","namespace":"kube-system"},"status":{"phase":"Running","containerStatuses":[{"ready":true}]}},
		{"metadata":{"name":"cilium","namespace":"kube-system"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
		{"metadata":{"name":"app","namespace":"default"},"status":{"phase":"Running","containerStatuses":[{"ready":false}]}}
	]}`)
	chk := systemWorkloadsCheck(raw, nil)
	if chk.Status != domain.HealthUnhealthy || !strings.Contains(chk.Summary, "cilium") {
		t.Errorf("crashloop → %+v, want unhealthy naming cilium", chk)
	}

	// A merely not-ready (no crash) kube-system pod is degraded, not unhealthy.
	degraded := []byte(`{"items":[
		{"metadata":{"name":"coredns","namespace":"kube-system"},"status":{"phase":"Running","containerStatuses":[{"ready":true}]}},
		{"metadata":{"name":"metrics-server","namespace":"kube-system"},"status":{"phase":"Pending","containerStatuses":[{"ready":false}]}}
	]}`)
	if chk := systemWorkloadsCheck(degraded, nil); chk.Status != domain.HealthDegraded {
		t.Errorf("not-ready → %s, want degraded", chk.Status)
	}

	healthy := []byte(`{"items":[{"metadata":{"name":"coredns","namespace":"kube-system"},"status":{"phase":"Running","containerStatuses":[{"ready":true}]}}]}`)
	if chk := systemWorkloadsCheck(healthy, nil); chk.Status != domain.HealthHealthy {
		t.Errorf("all ready → %s, want healthy", chk.Status)
	}

	// A flapping pod caught in its brief Running window (restarted repeatedly, not ready, no current
	// "waiting" state) must still read as unhealthy - the case a naive waiting-only check misses.
	flapping := []byte(`{"items":[
		{"metadata":{"name":"kube-proxy","namespace":"kube-system"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"restartCount":7}]}}
	]}`)
	if chk := systemWorkloadsCheck(flapping, nil); chk.Status != domain.HealthUnhealthy {
		t.Errorf("flapping (restarts, not ready) → %s, want unhealthy", chk.Status)
	}

	// A crash-looping init container (pod stuck Pending) is unhealthy too.
	initCrash := []byte(`{"items":[
		{"metadata":{"name":"cilium","namespace":"kube-system"},"status":{"phase":"Pending","initContainerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}],"containerStatuses":[{"ready":false}]}}
	]}`)
	if chk := systemWorkloadsCheck(initCrash, nil); chk.Status != domain.HealthUnhealthy {
		t.Errorf("init crashloop → %s, want unhealthy", chk.Status)
	}
}

func TestSchedulingCheck(t *testing.T) {
	// An unschedulable Pending pod is the hard signal → unhealthy.
	pending := []byte(`{"items":[
		{"metadata":{"name":"big","namespace":"default"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable"}]}}
	]}`)
	if chk := schedulingCheck(nil, pending, nil); chk.Status != domain.HealthUnhealthy || !strings.Contains(chk.Summary, "default/big") {
		t.Errorf("unschedulable → %+v, want unhealthy", chk)
	}

	// Node pressure alone is the soft signal → degraded.
	nodes := []node{{name: "w-0", ready: true, pressures: []string{"DiskPressure"}}}
	clean := []byte(`{"items":[]}`)
	if chk := schedulingCheck(nodes, clean, nil); chk.Status != domain.HealthDegraded || !strings.Contains(chk.Summary, "DiskPressure") {
		t.Errorf("pressure → %+v, want degraded", chk)
	}

	if chk := schedulingCheck(nil, clean, nil); chk.Status != domain.HealthHealthy {
		t.Errorf("clean → %s, want healthy", chk.Status)
	}

	// A cordoned node (kubectl cordon) is a warning - degraded - while other nodes can still take
	// work. Cordoning doesn't touch Ready, and doesn't necessarily produce Pending pods, so this is
	// the only check that catches it.
	oneCordoned := []node{{name: "cp-0", ready: true}, {name: "w-0", ready: true, cordoned: true}}
	if chk := schedulingCheck(oneCordoned, clean, nil); chk.Status != domain.HealthDegraded || !strings.Contains(chk.Summary, "cordoned: w-0") {
		t.Errorf("one cordoned → %+v, want degraded naming w-0", chk)
	}

	// Every node cordoned means nothing new can be scheduled anywhere → unhealthy, even with no
	// Pending pods yet.
	allCordoned := []node{{name: "cp-0", ready: true, cordoned: true}, {name: "w-0", ready: true, cordoned: true}}
	if chk := schedulingCheck(allCordoned, clean, nil); chk.Status != domain.HealthUnhealthy || !strings.Contains(chk.Summary, "cordoned") {
		t.Errorf("all cordoned → %+v, want unhealthy", chk)
	}
}

func TestAddonAvailabilityCheck(t *testing.T) {
	wanted := map[string]bool{"ingress-nginx": true, "prometheus": true}
	empty := []byte(`{"items":[]}`)
	// prometheus's Deployment is short a replica → that add-on is degraded (named); ingress is fine.
	deps := []byte(`{"items":[
		{"metadata":{"annotations":{"meta.helm.sh/release-name":"ingress-nginx"}},"spec":{"replicas":2},"status":{"availableReplicas":2}},
		{"metadata":{"annotations":{"meta.helm.sh/release-name":"prometheus"}},"spec":{"replicas":1},"status":{"availableReplicas":0}}
	]}`)
	chk := addonAvailabilityCheck(deps, nil, empty, nil, wanted)
	if chk.Status != domain.HealthDegraded || !strings.Contains(chk.Summary, "prometheus") {
		t.Errorf("one down → %+v, want degraded naming prometheus", chk)
	}
	if strings.Contains(chk.Summary, "ingress-nginx") {
		t.Errorf("healthy add-on should not be named as degraded: %q", chk.Summary)
	}

	// A Helm workload from a release NOT among the installed add-ons is ignored (scoping).
	otherRelease := []byte(`{"items":[{"metadata":{"annotations":{"meta.helm.sh/release-name":"something-else"}},"spec":{"replicas":1},"status":{"availableReplicas":0}}]}`)
	if chk := addonAvailabilityCheck(otherRelease, nil, empty, nil, wanted); chk.Status != domain.HealthUnknown {
		t.Errorf("only unrelated releases → %s, want unknown", chk.Status)
	}

	// Both add-ons' workloads healthy → healthy (a DaemonSet-backed add-on counts too).
	allUp := []byte(`{"items":[{"metadata":{"annotations":{"meta.helm.sh/release-name":"ingress-nginx"}},"spec":{"replicas":2},"status":{"availableReplicas":2}}]}`)
	allUpDS := []byte(`{"items":[{"metadata":{"annotations":{"meta.helm.sh/release-name":"prometheus"}},"status":{"desiredNumberScheduled":3,"numberAvailable":3}}]}`)
	if chk := addonAvailabilityCheck(allUp, nil, allUpDS, nil, wanted); chk.Status != domain.HealthHealthy || !strings.Contains(chk.Summary, "2/2") {
		t.Errorf("all up → %+v, want healthy 2/2", chk)
	}

	// No add-ons installed → unknown (not a spurious green).
	if chk := addonAvailabilityCheck(allUp, nil, empty, nil, map[string]bool{}); chk.Status != domain.HealthUnknown {
		t.Errorf("no add-ons installed → %s, want unknown", chk.Status)
	}
}

var errBoom = &boomErr{}

type boomErr struct{}

func (*boomErr) Error() string { return "boom" }
