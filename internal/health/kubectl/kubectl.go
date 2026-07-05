// Package kubectl is the real health.Checker: it evaluates a cluster's health by querying its API
// with `kubectl get --raw`, using the cluster's admin kubeconfig.
//
// Read-only and idempotent - the reconciler calls it on a slow ticker; a transient blip is simply
// retried next tick. An unreachable API server is reported as an unhealthy *check* (and the rest
// go "unknown"), not an error, so a broken cluster still gets a saved snapshot the portal can show.
// The only error path is a failure to prepare the kubeconfig.
package kubectl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/health"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kubeconfig"
	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
)

// Helm 3 stamps every resource it manages with this label, and records the release it belongs to
// in this annotation - so the add-on check can scope to exactly the cluster's add-on releases
// (each add-on is a Helm release named after the add-on; see internal/addons/helm) rather than
// every workload in the cluster.
const (
	helmManagedSelector   = "app.kubernetes.io/managed-by=Helm"
	helmReleaseAnnotation = "meta.helm.sh/release-name"
)

type Config struct {
	Bin          string       // "kubectl"
	WorkDir      string       // per-cluster dir for the temp kubeconfig
	KubeProxyURL string       // SOCKS proxy to reach the API server through; "" (local KVM) = direct
	Log          *slog.Logger // required
}

type Checker struct{ cfg Config }

func New(cfg Config) (*Checker, error) {
	if cfg.Bin == "" {
		cfg.Bin = "kubectl"
	}
	if cfg.WorkDir == "" {
		return nil, fmt.Errorf("kubectl: WorkDir is required")
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("kubectl: Log is required")
	}
	return &Checker{cfg: cfg}, nil
}

func (h *Checker) Check(ctx context.Context, c *domain.Cluster, kubeconfig []byte) (health.Result, error) {
	kcPath, err := h.writeKubeconfig(c.ID, kubeconfig)
	if err != nil {
		return health.Result{}, err
	}

	// API server first: everything else queries it, so if it's down there's nothing to evaluate.
	// Report it unhealthy and mark the rest unknown - still a saveable, informative snapshot.
	if _, err := h.raw(ctx, kcPath, "/readyz"); err != nil {
		return health.Result{Checks: []domain.HealthCheck{
			health.Check(health.CheckAPIServer, domain.HealthUnhealthy, "API server not responding"),
			health.Check(health.CheckNodesReady, domain.HealthUnknown, "API server unreachable"),
			health.Check(health.CheckSystemWorkloads, domain.HealthUnknown, "API server unreachable"),
			health.Check(health.CheckScheduling, domain.HealthUnknown, "API server unreachable"),
			health.Check(health.CheckEtcd, domain.HealthUnknown, "API server unreachable"),
			health.Check(health.CheckAddons, domain.HealthUnknown, "API server unreachable"),
			// Also derived from stored observed state, and pointed at exactly this situation: a
			// quota-exhausted etcd leaves the members up and in quorum while every write fails.
			health.EtcdStoreCheck(c),
			// Derived from stored observed expiry, not the live API - so it stays informative even
			// when the API server is down (arguably most useful then: an expired cert IS why it's down).
			health.CertCheck(c),
		}}, nil
	}

	// Add-on availability looks only at Helm-managed workloads, scoped to this cluster's add-on
	// releases - so filter server-side by the managed-by label.
	helmSel := "?labelSelector=" + url.QueryEscape(helmManagedSelector)

	nodesRaw, nodesErr := h.raw(ctx, kcPath, "/api/v1/nodes")
	podsRaw, podsErr := h.raw(ctx, kcPath, "/api/v1/pods")
	depRaw, depErr := h.raw(ctx, kcPath, "/apis/apps/v1/deployments"+helmSel)
	dsRaw, dsErr := h.raw(ctx, kcPath, "/apis/apps/v1/daemonsets"+helmSel)
	_, etcdErr := h.raw(ctx, kcPath, "/livez/etcd")

	nodes, nodeHealth := parseNodes(nodesRaw, nodesErr)

	checks := []domain.HealthCheck{
		health.Check(health.CheckAPIServer, domain.HealthHealthy, "control plane responding"),
		nodesReadyCheck(nodes, nodesErr),
		systemWorkloadsCheck(podsRaw, podsErr),
		schedulingCheck(nodes, podsRaw, podsErr),
		etcdCheck(c, etcdErr),
		// The backend-store counterpart to the quorum probe above: same component, but read from the
		// size/fragmentation/alarms the reconciler stamped rather than from /livez/etcd, which stays
		// green on a cluster that has hit its quota and gone read-only.
		health.EtcdStoreCheck(c),
		addonAvailabilityCheck(depRaw, depErr, dsRaw, dsErr, installedAddonReleases(c)),
		// Derived from the expiry the reconciler observed and stamped on the cluster (not a live
		// kubectl probe - kubeadm's PKI isn't visible through the API), so it matches the fake.
		health.CertCheck(c),
	}
	return health.Result{Checks: checks, Nodes: nodeHealth}, nil
}

// installedAddonReleases is the set of Helm release names for the cluster's currently-installed
// add-ons (each add-on is a release named after itself). Used to scope the add-on availability
// check to this cluster's add-ons and ignore any other Helm releases in the cluster.
func installedAddonReleases(c *domain.Cluster) map[string]bool {
	out := make(map[string]bool, len(c.Addons))
	for _, a := range c.Addons {
		if a.Phase == "installed" {
			out[a.Name] = true
		}
	}
	return out
}

func (h *Checker) raw(ctx context.Context, kcPath, path string) ([]byte, error) {
	swallow := func(string) {} // stderr is only interesting when the command actually fails
	return procstream.Capture(ctx, "", os.Environ(), swallow, h.cfg.Bin,
		"--kubeconfig", kcPath, "get", "--raw", path)
}

// --- nodes ------------------------------------------------------------------

// node is the slice of /api/v1/nodes we care about: the Ready condition, the pressure ones, and
// whether it's cordoned (spec.unschedulable - set by `kubectl cordon`/drain).
type node struct {
	name      string
	ready     bool
	cordoned  bool
	pressures []string
}

var pressureTypes = map[string]bool{"MemoryPressure": true, "DiskPressure": true, "PIDPressure": true}

// parseNodes decodes /api/v1/nodes into our node view plus the per-node health carried on the
// snapshot. On a decode/query error it returns nils, and the dependent checks report unknown.
func parseNodes(b []byte, err error) ([]node, []domain.NodeHealth) {
	if err != nil {
		return nil, nil
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(b, &list) != nil {
		return nil, nil
	}
	nodes := make([]node, 0, len(list.Items))
	nh := make([]domain.NodeHealth, 0, len(list.Items))
	for _, it := range list.Items {
		n := node{name: it.Metadata.Name, cordoned: it.Spec.Unschedulable}
		for _, cond := range it.Status.Conditions {
			switch {
			case cond.Type == "Ready":
				n.ready = cond.Status == "True"
			case pressureTypes[cond.Type] && cond.Status == "True":
				n.pressures = append(n.pressures, cond.Type)
			}
		}
		nodes = append(nodes, n)
		nh = append(nh, domain.NodeHealth{
			NodeName: n.name, Ready: n.ready, Cordoned: n.cordoned, Pressures: n.pressures,
		})
	}
	return nodes, nh
}

func nodesReadyCheck(nodes []node, err error) domain.HealthCheck {
	if err != nil || nodes == nil {
		return health.Check(health.CheckNodesReady, domain.HealthUnknown, "could not read nodes")
	}
	ready, notReady := 0, []string{}
	for _, n := range nodes {
		if n.ready {
			ready++
		} else {
			notReady = append(notReady, n.name)
		}
	}
	total := len(nodes)
	if len(notReady) > 0 {
		return health.Check(health.CheckNodesReady, domain.HealthUnhealthy,
			fmt.Sprintf("%d/%d Ready - NotReady: %s", ready, total, strings.Join(truncate(notReady, 3), ", ")))
	}
	return health.Check(health.CheckNodesReady, domain.HealthHealthy, fmt.Sprintf("%d/%d nodes Ready", ready, total))
}

// --- pods (system workloads + scheduling) -----------------------------------

type podList struct {
	Items []pod `json:"items"`
}

type containerStatus struct {
	Ready        bool `json:"ready"`
	RestartCount int  `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
	} `json:"state"`
}

type pod struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"conditions"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

// crashing reports whether any of the pod's containers (init or main) is crash-looping. It catches
// both the explicit waiting-backoff states AND a container that has restarted repeatedly and isn't
// currently ready - the latter covers the brief window where a flapping container momentarily shows
// a Running state (so it isn't "waiting" at the instant we sampled) yet is plainly unhealthy.
func (p pod) crashing() bool {
	for _, group := range [][]containerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, cs := range group {
			if cs.State.Waiting != nil {
				switch cs.State.Waiting.Reason {
				case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull",
					"CreateContainerConfigError", "CreateContainerError", "RunContainerError":
					return true
				}
			}
			if cs.RestartCount >= 2 && !cs.Ready {
				return true
			}
		}
	}
	return false
}

// containersReady reports whether every container in the pod is ready.
func (p pod) containersReady() bool {
	if len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

// unschedulable reports whether the pod is Pending because the scheduler can't place it.
func (p pod) unschedulable() bool {
	if p.Status.Phase != "Pending" {
		return false
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == "PodScheduled" && cond.Status == "False" && cond.Reason == "Unschedulable" {
			return true
		}
	}
	return false
}

// systemWorkloadsCheck evaluates the kube-system pods (CoreDNS, kube-proxy, the CNI, metrics-server):
// crash-looping is unhealthy, merely not-ready is degraded, everything Running+ready is healthy.
func systemWorkloadsCheck(b []byte, err error) domain.HealthCheck {
	if err != nil {
		return health.Check(health.CheckSystemWorkloads, domain.HealthUnknown, "could not read pods")
	}
	var list podList
	if json.Unmarshal(b, &list) != nil {
		return health.Check(health.CheckSystemWorkloads, domain.HealthUnknown, "could not read pods")
	}
	total, ready := 0, 0
	var crashing, notReady []string
	for _, p := range list.Items {
		if p.Metadata.Namespace != "kube-system" {
			continue
		}
		total++
		switch {
		case p.Status.Phase == "Succeeded":
			ready++ // a completed pod (e.g. a one-shot job) is fine
		case p.crashing():
			crashing = append(crashing, p.Metadata.Name)
		case p.Status.Phase == "Running" && p.containersReady():
			ready++
		default:
			notReady = append(notReady, p.Metadata.Name)
		}
	}
	switch {
	case total == 0:
		return health.Check(health.CheckSystemWorkloads, domain.HealthUnknown, "no kube-system pods found")
	case len(crashing) > 0:
		return health.Check(health.CheckSystemWorkloads, domain.HealthUnhealthy,
			fmt.Sprintf("%d/%d ready - crash-looping: %s", ready, total, strings.Join(truncate(crashing, 3), ", ")))
	case len(notReady) > 0:
		return health.Check(health.CheckSystemWorkloads, domain.HealthDegraded,
			fmt.Sprintf("%d/%d ready - not ready: %s", ready, total, strings.Join(truncate(notReady, 3), ", ")))
	}
	return health.Check(health.CheckSystemWorkloads, domain.HealthHealthy, fmt.Sprintf("%d/%d kube-system pods ready", ready, total))
}

// schedulingCheck reports whether the cluster can still place new work. Unschedulable Pending pods
// are the hardest signal, and so is every node being cordoned (spec.unschedulable - `kubectl
// cordon`/drain): either means new work has nowhere to go, so both are unhealthy. A subset of nodes
// cordoned, or node MemoryPressure/DiskPressure/PIDPressure, only reduces capacity - degraded.
// Everything clear → healthy. Cordoning alone doesn't touch a node's Ready condition, so it would
// otherwise go unnoticed by every other check.
func schedulingCheck(nodes []node, podsRaw []byte, podsErr error) domain.HealthCheck {
	if podsErr != nil {
		return health.Check(health.CheckScheduling, domain.HealthUnknown, "could not read pods")
	}
	var list podList
	if json.Unmarshal(podsRaw, &list) != nil {
		return health.Check(health.CheckScheduling, domain.HealthUnknown, "could not read pods")
	}
	var pending []string
	for _, p := range list.Items {
		if p.unschedulable() {
			pending = append(pending, p.Metadata.Namespace+"/"+p.Metadata.Name)
		}
	}
	var pressured, cordoned []string
	for _, n := range nodes {
		for _, pr := range n.pressures {
			pressured = append(pressured, n.name+" "+pr)
		}
		if n.cordoned {
			cordoned = append(cordoned, n.name)
		}
	}
	allCordoned := len(nodes) > 0 && len(cordoned) == len(nodes)

	switch {
	case len(pending) > 0:
		return health.Check(health.CheckScheduling, domain.HealthUnhealthy,
			fmt.Sprintf("%d pod(s) unschedulable: %s", len(pending), strings.Join(truncate(pending, 2), ", ")))
	case allCordoned:
		return health.Check(health.CheckScheduling, domain.HealthUnhealthy,
			fmt.Sprintf("all %d node(s) cordoned - no scheduling capacity", len(nodes)))
	}

	var warnings []string
	if len(cordoned) > 0 {
		warnings = append(warnings, fmt.Sprintf("cordoned: %s", strings.Join(truncate(cordoned, 3), ", ")))
	}
	if len(pressured) > 0 {
		warnings = append(warnings, fmt.Sprintf("node pressure: %s", strings.Join(truncate(pressured, 3), ", ")))
	}
	if len(warnings) > 0 {
		return health.Check(health.CheckScheduling, domain.HealthDegraded, strings.Join(warnings, "; "))
	}
	return health.Check(health.CheckScheduling, domain.HealthHealthy, "no unschedulable pods; no node pressure or cordons")
}

// --- etcd -------------------------------------------------------------------

// etcdCheck maps the API server's own etcd liveness probe (/livez/etcd) to a health status. The
// member count is the expected control-plane count; a real member-list would need to exec into
// etcd, which the read-only kubectl seam can't do (production would scrape etcd's own metrics).
func etcdCheck(c *domain.Cluster, err error) domain.HealthCheck {
	quorum := fmt.Sprintf("%d-member quorum", c.ControlPlaneCount())
	if err != nil {
		return health.Check(health.CheckEtcd, domain.HealthUnhealthy, "etcd liveness probe failing")
	}
	return health.Check(health.CheckEtcd, domain.HealthHealthy, "etcd healthy ("+quorum+")")
}

// --- add-on availability ----------------------------------------------------

// addonAvailabilityCheck rolls up the availability of the cluster's add-ons, grouped by Helm
// release: a Deployment short of its available replicas, or a DaemonSet short of its desired pods,
// is not available, and an add-on is degraded if any of its workloads is. Only workloads belonging
// to an *installed* add-on release (`wanted`) are considered - Helm-managed workloads from other
// releases, and core controllers, are ignored. An add-on with no observed apps/v1 workloads yet
// (still installing, or CRD-only) simply isn't counted rather than failed.
func addonAvailabilityCheck(depRaw []byte, depErr error, dsRaw []byte, dsErr error, wanted map[string]bool) domain.HealthCheck {
	if depErr != nil || dsErr != nil {
		return health.Check(health.CheckAddons, domain.HealthUnknown, "could not read workloads")
	}
	if len(wanted) == 0 {
		return health.Check(health.CheckAddons, domain.HealthUnknown, "no add-ons installed")
	}
	workloads, ok := parseHelmWorkloads(depRaw, dsRaw)
	if !ok {
		return health.Check(health.CheckAddons, domain.HealthUnknown, "could not read workloads")
	}

	// Tally per release, scoped to the cluster's installed add-ons.
	type tally struct{ total, up int }
	rel := map[string]*tally{}
	for _, w := range workloads {
		if !wanted[w.releaseName] {
			continue
		}
		t := rel[w.releaseName]
		if t == nil {
			t = &tally{}
			rel[w.releaseName] = t
		}
		t.total++
		if w.available {
			t.up++
		}
	}
	if len(rel) == 0 {
		return health.Check(health.CheckAddons, domain.HealthUnknown, "no add-on workloads found")
	}
	var down []string
	for name, t := range rel {
		if t.up < t.total {
			down = append(down, name)
		}
	}
	if len(down) > 0 {
		return health.Check(health.CheckAddons, domain.HealthDegraded,
			fmt.Sprintf("%d/%d add-ons available - degraded: %s", len(rel)-len(down), len(rel), strings.Join(truncate(down, 3), ", ")))
	}
	return health.Check(health.CheckAddons, domain.HealthHealthy, fmt.Sprintf("%d/%d add-ons available", len(rel), len(rel)))
}

// helmWorkload is one apps/v1 workload attributed to its Helm release, with whether it is fully
// available (Deployment replicas / DaemonSet pods).
type helmWorkload struct {
	releaseName string
	available   bool
}

// parseHelmWorkloads decodes the (label-filtered) Deployment and DaemonSet lists, reading each
// workload's owning Helm release from its annotation. Deployments/DaemonSets desiring zero pods are
// skipped (not a health signal). Returns false only on a decode error.
func parseHelmWorkloads(depRaw, dsRaw []byte) ([]helmWorkload, bool) {
	type meta struct {
		Annotations map[string]string `json:"annotations"`
	}
	var deps struct {
		Items []struct {
			Metadata meta `json:"metadata"`
			Spec     struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				AvailableReplicas int `json:"availableReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	var dss struct {
		Items []struct {
			Metadata meta `json:"metadata"`
			Status   struct {
				DesiredNumberScheduled int `json:"desiredNumberScheduled"`
				NumberAvailable        int `json:"numberAvailable"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(depRaw, &deps) != nil || json.Unmarshal(dsRaw, &dss) != nil {
		return nil, false
	}
	var out []helmWorkload
	for _, d := range deps.Items {
		want := 1
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if want == 0 {
			continue
		}
		out = append(out, helmWorkload{
			releaseName: d.Metadata.Annotations[helmReleaseAnnotation],
			available:   d.Status.AvailableReplicas >= want,
		})
	}
	for _, d := range dss.Items {
		if d.Status.DesiredNumberScheduled == 0 {
			continue
		}
		out = append(out, helmWorkload{
			releaseName: d.Metadata.Annotations[helmReleaseAnnotation],
			available:   d.Status.NumberAvailable >= d.Status.DesiredNumberScheduled,
		})
	}
	return out, true
}

// --- helpers ----------------------------------------------------------------

// truncate returns at most n names, appending an "+N more" marker when it clipped, so summaries
// stay short. Sorted for stable output.
func truncate(names []string, n int) []string {
	sort.Strings(names)
	if len(names) <= n {
		return names
	}
	out := append([]string(nil), names[:n]...)
	return append(out, fmt.Sprintf("+%d more", len(names)-n))
}

func (h *Checker) writeKubeconfig(clusterID string, kc []byte) (string, error) {
	dir := filepath.Join(h.cfg.WorkDir, clusterID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Routes the health checks through the KVM host when the cluster isn't locally reachable; no-op
	// when it is (see internal/kubeconfig).
	kc, err := kubeconfig.WithProxy(kc, h.cfg.KubeProxyURL)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, kc, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
