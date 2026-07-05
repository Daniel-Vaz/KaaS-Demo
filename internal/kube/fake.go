package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Fake is the in-process kube.Client used in fake mode (make up-fake): it synthesizes a plausible
// set of workloads, pods, events, YAML and logs from a cluster's control-plane state, so the whole
// Workloads page is demoable with no KVM - mirroring every other fake seam. Scaling is honored in an
// in-memory override map (keyed per cluster+workload) so a scale visibly changes the replica count
// within the process, and read-only sessions are refused just as the viewer kubeconfig's RBAC would.
type Fake struct {
	mu     sync.Mutex
	scaled map[string]int // ref key -> overridden replica count
}

// NewFake returns the fake kube client.
func NewFake() *Fake { return &Fake{scaled: map[string]int{}} }

func refKey(clusterID string, ref WorkloadRef) string {
	return clusterID + "|" + string(ref.Kind) + "|" + ref.Namespace + "|" + ref.Name
}

func (f *Fake) override(clusterID string, ref WorkloadRef, desired int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.scaled[refKey(clusterID, ref)]; ok {
		return v
	}
	return desired
}

// MintUserKubeconfig synthesizes a plausible per-user kubeconfig without a real API server, so the
// portal's Download button demos under make up-fake. It embeds the resolved identity (CN=username,
// O=the role's kube group) as comments, targets the control-plane IP, and reports an expiry ttl out.
func (f *Fake) MintUserKubeconfig(_ context.Context, c *domain.Cluster, _ []byte, username string, role domain.GroupRole, ttl time.Duration) ([]byte, time.Time, error) {
	var cpIP string
	for _, n := range c.Nodes {
		if n.Role == domain.RoleControlPlane {
			cpIP = n.IP
		}
	}
	group := domain.KubeGroupForRole(role)
	notAfter := time.Now().Add(ttl)
	kubeconfig := fmt.Appendf(nil,
		"apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://%s:6443\n  name: %s\nusers:\n- name: %s\n  user:\n    # NOTE: fake per-user client-cert kubeconfig - CN=%s, O=%s, expires %s\n    client-certificate-data: ZmFrZQ==\n    client-key-data: ZmFrZQ==\ncontexts:\n- name: %s@%s\n  context:\n    cluster: %s\n    user: %s\ncurrent-context: %s@%s\n",
		cpIP, c.Name, username, username, group, notAfter.Format("2006-01-02"), username, c.Name, c.Name, username, username, c.Name)
	return kubeconfig, notAfter, nil
}

func (f *Fake) Namespaces(_ context.Context, c *domain.Cluster, _ []byte) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(ns string) {
		if ns != "" && !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	for _, ns := range []string{"default", "kube-node-lease", "kube-public", "kube-system", "demo"} {
		add(ns)
	}
	for _, w := range f.build(c) {
		add(w.namespace)
	}
	// The namespace picker is shared with the Storage page, so a namespace that holds only claims
	// (an add-on's, say) must be offered too - otherwise its claims can't be filtered to.
	for _, p := range f.buildPVCs(c) {
		add(p.namespace)
	}
	sort.Strings(out)
	return out, nil
}

func (f *Fake) Workloads(_ context.Context, c *domain.Cluster, _ []byte, namespace string) ([]WorkloadSummary, error) {
	var out []WorkloadSummary
	for _, w := range f.build(c) {
		if namespace != "" && w.namespace != namespace {
			continue
		}
		out = append(out, f.summary(c, w))
	}
	return out, nil
}

func (f *Fake) Workload(_ context.Context, c *domain.Cluster, _ []byte, ref WorkloadRef) (*WorkloadDetail, error) {
	for _, w := range f.build(c) {
		if w.kind == ref.Kind && w.namespace == ref.Namespace && w.name == ref.Name {
			d := f.detail(c, w)
			return &d, nil
		}
	}
	return nil, fmt.Errorf("workload %s/%s not found", ref.Namespace, ref.Name)
}

func (f *Fake) Manifest(_ context.Context, c *domain.Cluster, _ []byte, ref WorkloadRef) (string, error) {
	for _, w := range f.build(c) {
		if w.kind == ref.Kind && w.namespace == ref.Namespace && w.name == ref.Name {
			return f.manifest(c, w), nil
		}
	}
	return "", fmt.Errorf("workload %s/%s not found", ref.Namespace, ref.Name)
}

func (f *Fake) Events(_ context.Context, c *domain.Cluster, _ []byte, ref WorkloadRef) ([]Event, error) {
	for _, w := range f.build(c) {
		if w.kind == ref.Kind && w.namespace == ref.Namespace && w.name == ref.Name {
			return f.events(c, w), nil
		}
	}
	return nil, fmt.Errorf("workload %s/%s not found", ref.Namespace, ref.Name)
}

func (f *Fake) Scale(_ context.Context, c *domain.Cluster, _ []byte, ref WorkloadRef, replicas int, readOnly bool) error {
	if readOnly {
		return fmt.Errorf("scaling %s/%s is not permitted - you have read-only (view) access to this cluster", ref.Namespace, ref.Name)
	}
	if !ref.Kind.Scalable() {
		return fmt.Errorf("%s workloads cannot be scaled by replicas", ref.Kind)
	}
	if replicas < 0 {
		return fmt.Errorf("replicas must be >= 0")
	}
	f.mu.Lock()
	f.scaled[refKey(c.ID, ref)] = replicas
	f.mu.Unlock()
	return nil
}

// Logs synthesizes a plausible log stream for the pod. With Follow it keeps emitting a new line
// roughly once a second until ctx is cancelled; otherwise it prints the tail and returns.
func (f *Fake) Logs(ctx context.Context, _ *domain.Cluster, _ []byte, ref LogRef, sink LogSink) error {
	n := ref.TailLines
	if n <= 0 || n > 200 {
		n = 40
	}
	start := time.Now().Add(-time.Duration(n) * time.Second)
	for i := 0; i < n; i++ {
		if err := sink.Write([]byte(fakeLogLine(ref, start.Add(time.Duration(i)*time.Second), i))); err != nil {
			return err
		}
	}
	if !ref.Follow {
		return nil
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	i := n
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := sink.Write([]byte(fakeLogLine(ref, time.Now(), i))); err != nil {
				return err
			}
			i++
		}
	}
}

// fakeLogLine produces one deterministic-ish access/app log line for the pod.
func fakeLogLine(ref LogRef, ts time.Time, i int) string {
	paths := []string{"/", "/healthz", "/api/v1/items", "/metrics", "/favicon.ico"}
	codes := []int{200, 200, 200, 204, 200, 301, 404}
	comp := ref.Container
	if comp == "" {
		comp = "app"
	}
	return fmt.Sprintf("%s [%s] %s \"GET %s HTTP/1.1\" %d %dms\n",
		ts.UTC().Format("2006-01-02T15:04:05.000Z"), comp, ref.Pod,
		paths[i%len(paths)], codes[i%len(codes)], 3+(i*7)%180)
}

// ---- synthesized workload model ------------------------------------------------

type fakeWorkload struct {
	kind       WorkloadKind
	namespace  string
	name       string
	replicas   int // desired for Deployment/StatefulSet; ignored for DS/Job/CronJob
	containers []Container
	schedule   string // CronJob
	suspended  bool   // CronJob
	strategy   string
	selector   map[string]string
	// dsAllNodes: DaemonSet runs one pod per node.
	dsAllNodes bool
	// jobDone: a completed one-shot Job (1/1 succeeded, no live pods but one Completed pod).
	jobDone bool
}

// build returns the full synthesized workload set for a cluster: the core system controllers, one
// per pod-bearing add-on, and a few demo workloads so the page is rich. Deterministic in cluster
// state, so it is stable across the portal's polling.
func (f *Fake) build(c *domain.Cluster) []fakeWorkload {
	k8s := c.K8sVersion
	cni := c.CNI
	if cni == "" {
		cni = "cni"
	}
	sel := func(app string) map[string]string { return map[string]string{"k8s-app": app} }

	ws := []fakeWorkload{
		{
			kind: KindDeployment, namespace: "kube-system", name: "coredns", replicas: 2,
			containers: []Container{{Name: "coredns", Image: "registry.k8s.io/coredns/coredns:v1.11.1"}},
			strategy:   "RollingUpdate", selector: sel("kube-dns"),
		},
		{
			kind: KindDaemonSet, namespace: "kube-system", name: "kube-proxy", dsAllNodes: true,
			containers: []Container{{Name: "kube-proxy", Image: "registry.k8s.io/kube-proxy:v" + k8s}},
			strategy:   "RollingUpdate", selector: sel("kube-proxy"),
		},
		{
			kind: KindDaemonSet, namespace: "kube-system", name: cni, dsAllNodes: true,
			containers: []Container{{Name: cni + "-agent", Image: cniImage(cni, c.CNIVersion)}},
			strategy:   "RollingUpdate", selector: sel(cni),
		},
	}

	// One controller per pod-bearing add-on (skip the CNI add-on, represented above).
	for _, a := range c.Addons {
		if a.Phase == "removing" || a.Name == c.CNI {
			continue
		}
		switch a.Name {
		case "metrics-server":
			ws = append(ws, fakeWorkload{
				kind: KindDeployment, namespace: "kube-system", name: "metrics-server", replicas: 1,
				containers: []Container{{Name: "metrics-server", Image: "registry.k8s.io/metrics-server/metrics-server:v0.7.2"}},
				strategy:   "RollingUpdate", selector: sel("metrics-server"),
			})
		case "ingress-nginx":
			ws = append(ws, fakeWorkload{
				kind: KindDeployment, namespace: "ingress-nginx", name: "ingress-nginx-controller", replicas: 2,
				containers: []Container{{Name: "controller", Image: "registry.k8s.io/ingress-nginx/controller:v1.11.2"}},
				strategy:   "RollingUpdate", selector: map[string]string{"app.kubernetes.io/name": "ingress-nginx"},
			})
		}
	}

	// Demo application workloads (in the "demo" namespace) so every kind is represented.
	ws = append(ws,
		fakeWorkload{
			kind: KindDeployment, namespace: "demo", name: "web", replicas: 3,
			containers: []Container{{Name: "nginx", Image: "nginx:1.27"}},
			strategy:   "RollingUpdate", selector: map[string]string{"app": "web"},
		},
		fakeWorkload{
			kind: KindStatefulSet, namespace: "demo", name: "cache", replicas: 2,
			containers: []Container{{Name: "redis", Image: "redis:7.2"}},
			strategy:   "RollingUpdate", selector: map[string]string{"app": "cache"},
		},
		fakeWorkload{
			kind: KindCronJob, namespace: "demo", name: "report-generator",
			schedule: "*/5 * * * *", suspended: false,
			containers: []Container{{Name: "report", Image: "busybox:1.36"}},
			selector:   map[string]string{"app": "report-generator"},
		},
		fakeWorkload{
			kind: KindJob, namespace: "demo", name: "db-migrate", jobDone: true,
			containers: []Container{{Name: "migrate", Image: "migrate/migrate:v4.17.0"}},
			selector:   map[string]string{"job-name": "db-migrate"},
		},
	)
	return ws
}

func cniImage(cni, ver string) string {
	if ver == "" {
		ver = "1.16.1"
	}
	switch cni {
	case "cilium":
		return "quay.io/cilium/cilium:v" + ver
	case "calico":
		return "docker.io/calico/node:v" + ver
	case "flannel":
		return "docker.io/flannel/flannel:v" + ver
	default:
		return cni + ":v" + ver
	}
}

// desiredReplicas resolves the effective desired count, honoring any in-memory scale override.
func (f *Fake) desiredReplicas(c *domain.Cluster, w fakeWorkload) int {
	switch w.kind {
	case KindDaemonSet:
		return len(c.Nodes)
	case KindJob:
		return 1
	case KindCronJob:
		return 0
	default:
		return f.override(c.ID, WorkloadRef{Kind: w.kind, Namespace: w.namespace, Name: w.name}, w.replicas)
	}
}

func (f *Fake) summary(c *domain.Cluster, w fakeWorkload) WorkloadSummary {
	desired := f.desiredReplicas(c, w)
	return WorkloadSummary{
		Kind:            w.kind,
		Namespace:       w.namespace,
		Name:            w.name,
		ReadyReplicas:   desired, // fake: everything is healthy
		DesiredReplicas: desired,
		Status:          f.status(w, desired),
		Images:          images(w),
		CreatedAt:       c.CreatedAt,
		Schedule:        w.schedule,
		Suspended:       w.suspended,
	}
}

func (f *Fake) status(w fakeWorkload, desired int) string {
	switch w.kind {
	case KindJob:
		return "Complete"
	case KindCronJob:
		if w.suspended {
			return "Suspended"
		}
		return "Scheduled"
	default:
		if desired == 0 {
			return "Scaled to zero"
		}
		return "Running"
	}
}

func images(w fakeWorkload) []string {
	out := make([]string, 0, len(w.containers))
	for _, ct := range w.containers {
		out = append(out, ct.Image)
	}
	return out
}

func (f *Fake) detail(c *domain.Cluster, w fakeWorkload) WorkloadDetail {
	sum := f.summary(c, w)
	d := WorkloadDetail{
		WorkloadSummary:   sum,
		UpdatedReplicas:   sum.ReadyReplicas,
		AvailableReplicas: sum.ReadyReplicas,
		Strategy:          w.strategy,
		Selector:          w.selector,
		Labels:            w.selector,
		Containers:        w.containers,
		Conditions:        f.conditions(w),
		Pods:              f.pods(c, w, sum.DesiredReplicas),
	}
	return d
}

func (f *Fake) conditions(w fakeWorkload) []Condition {
	now := time.Now()
	switch w.kind {
	case KindJob:
		return []Condition{{Type: "Complete", Status: "True", Reason: "", Updated: now}}
	case KindCronJob:
		return nil
	default:
		return []Condition{
			{Type: "Available", Status: "True", Reason: "MinimumReplicasAvailable", Updated: now},
			{Type: "Progressing", Status: "True", Reason: "NewReplicaSetAvailable", Updated: now},
		}
	}
}

func (f *Fake) pods(c *domain.Cluster, w fakeWorkload, desired int) []PodInfo {
	names := make([]string, 0, len(w.containers))
	for _, ct := range w.containers {
		names = append(names, ct.Name)
	}
	// Nodes a pod can land on: workers if any, else all nodes.
	var workers []domain.Node
	for _, n := range c.Nodes {
		if n.Role != domain.RoleControlPlane {
			workers = append(workers, n)
		}
	}
	pool := workers
	if len(pool) == 0 {
		pool = c.Nodes
	}

	var out []PodInfo
	add := func(podName, node, ip, status string, restarts int) {
		out = append(out, PodInfo{
			Name: podName, Ready: fmt.Sprintf("%d/%d", len(names), len(names)),
			Status: status, Restarts: restarts, Node: node, IP: ip,
			CreatedAt: c.CreatedAt, Containers: names,
		})
	}
	podNode := func(i int) (string, string) {
		if len(pool) == 0 {
			return "", ""
		}
		n := pool[i%len(pool)]
		return n.VMName, podIP(c, n, w.name, i)
	}

	switch w.kind {
	case KindDaemonSet:
		for i, n := range c.Nodes {
			add(w.name+"-"+randSuffix(w.name+n.VMName, i), n.VMName, podIP(c, n, w.name, i), "Running", 0)
		}
	case KindJob:
		node, ip := podNode(0)
		add(w.name+"-"+randSuffix(w.name, 0), node, ip, "Completed", 0)
	case KindCronJob:
		// No steady-state pods for a scheduled CronJob between runs.
	default:
		rs := randSuffix(w.name, 7) // stable ReplicaSet-style hash
		for i := 0; i < desired; i++ {
			node, ip := podNode(i)
			add(fmt.Sprintf("%s-%s-%s", w.name, rs, randSuffix(w.name, i)), node, ip, "Running", 0)
		}
	}
	return out
}

// podIP fabricates a stable pod IP inside a plausible pod CIDR for the node.
func podIP(c *domain.Cluster, n domain.Node, name string, i int) string {
	base := c.PodCIDR
	if base == "" {
		base = "10.244.0.0/16"
	}
	prefix := strings.SplitN(base, ".", 3)
	if len(prefix) < 2 {
		return ""
	}
	octet := (int(hash32(n.VMName)) % 250) + 1
	host := (int(hash32(name)) + i) % 250
	return fmt.Sprintf("%s.%s.%d.%d", prefix[0], prefix[1], octet, host)
}

func (f *Fake) events(c *domain.Cluster, w fakeWorkload) []Event {
	now := time.Now()
	obj := kindPretty(w.kind) + "/" + w.name
	switch w.kind {
	case KindJob:
		return []Event{
			{Type: "Normal", Reason: "SuccessfulCreate", Message: "Created pod: " + w.name + "-" + randSuffix(w.name, 0), Count: 1, LastSeen: c.CreatedAt, Object: obj},
			{Type: "Normal", Reason: "Completed", Message: "Job completed", Count: 1, LastSeen: c.CreatedAt.Add(30 * time.Second), Object: obj},
		}
	case KindCronJob:
		return []Event{
			{Type: "Normal", Reason: "SawCompletedJob", Message: "Saw completed job: " + w.name + "-28000000", Count: 12, LastSeen: now.Add(-2 * time.Minute), Object: obj},
		}
	default:
		return []Event{
			{Type: "Normal", Reason: "ScalingReplicaSet", Message: fmt.Sprintf("Scaled up replica set %s-%s to %d", w.name, randSuffix(w.name, 7), f.desiredReplicas(c, w)), Count: 1, LastSeen: c.CreatedAt, Object: obj},
		}
	}
}

// manifest renders a compact, readable YAML for the workload (not a full API object - enough to be
// convincing in fake mode; real mode returns the cluster's actual YAML).
func (f *Fake) manifest(c *domain.Cluster, w fakeWorkload) string {
	desired := f.desiredReplicas(c, w)
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\n", apiVersionFor(w.kind))
	fmt.Fprintf(&b, "kind: %s\n", kindPretty(w.kind))
	b.WriteString("metadata:\n")
	fmt.Fprintf(&b, "  name: %s\n", w.name)
	fmt.Fprintf(&b, "  namespace: %s\n", w.namespace)
	fmt.Fprintf(&b, "  creationTimestamp: %q\n", c.CreatedAt.UTC().Format(time.RFC3339))
	if len(w.selector) > 0 {
		b.WriteString("  labels:\n")
		for _, k := range sortedKeys(w.selector) {
			fmt.Fprintf(&b, "    %s: %s\n", k, w.selector[k])
		}
	}
	b.WriteString("spec:\n")
	switch w.kind {
	case KindCronJob:
		fmt.Fprintf(&b, "  schedule: %q\n", w.schedule)
		fmt.Fprintf(&b, "  suspend: %t\n", w.suspended)
	case KindDaemonSet:
		// no replicas
	default:
		fmt.Fprintf(&b, "  replicas: %d\n", desired)
	}
	if len(w.selector) > 0 {
		b.WriteString("  selector:\n    matchLabels:\n")
		for _, k := range sortedKeys(w.selector) {
			fmt.Fprintf(&b, "      %s: %s\n", k, w.selector[k])
		}
	}
	b.WriteString("  template:\n    spec:\n      containers:\n")
	for _, ct := range w.containers {
		fmt.Fprintf(&b, "        - name: %s\n          image: %s\n", ct.Name, ct.Image)
	}
	return b.String()
}

// ---- small helpers -------------------------------------------------------------

func kindPretty(k WorkloadKind) string {
	switch k {
	case KindDeployment:
		return "Deployment"
	case KindStatefulSet:
		return "StatefulSet"
	case KindDaemonSet:
		return "DaemonSet"
	case KindJob:
		return "Job"
	case KindCronJob:
		return "CronJob"
	default:
		return string(k)
	}
}

func apiVersionFor(k WorkloadKind) string {
	switch k {
	case KindJob:
		return "batch/v1"
	case KindCronJob:
		return "batch/v1"
	default:
		return "apps/v1"
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hash32(s string) uint32 {
	h := uint32(2166136261)
	for _, r := range s {
		h = (h ^ uint32(r)) * 16777619
	}
	return h
}

// randSuffix returns a short deterministic hex suffix seeded by s+i, so synthesized names look real
// and stay stable across polls within a process (cosmetic only, no crypto need).
func randSuffix(s string, i int) string {
	const hex = "0123456789abcdef"
	h := hash32(s) ^ uint32(i*2654435761)
	var b [5]byte
	for j := range b {
		b[j] = hex[h&0xf]
		h >>= 4
	}
	return string(b[:])
}
