// Package kubectl is the real kube.Client: it lists and inspects a cluster's live workloads by
// shelling out to `kubectl ... -o json` (and `kubectl logs -f` for streaming), using the cluster's
// kubeconfig. The command execution is abstracted behind an Execer so the identical arg-building and
// JSON parsing runs both on the worker (LocalExecer, which runs kubectl directly - the only place
// with a route to the cluster API server) and on the API (the proxy Execer in internal/kube/proxy,
// which forwards each invocation to the worker exec agent). See internal/kube for the seam overview.
package kubectl

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
)

// Result is the outcome of one kubectl invocation.
type Result struct {
	Stdout []byte
	Stderr string
	Code   int
}

// Execer runs kubectl for a cluster against its kubeconfig. Run captures a one-shot command's
// output; Stream pipes a long-running command's stdout (used for `kubectl logs -f`) into sink line
// by line until the command exits or ctx is cancelled. Both receive args WITHOUT --kubeconfig; the
// executor injects it (writing the kubeconfig to a private temp file on whichever host runs kubectl).
type Execer interface {
	Run(ctx context.Context, kubeconfig []byte, clusterID string, args []string) (Result, error)
	Stream(ctx context.Context, kubeconfig []byte, clusterID string, args []string, sink kube.LogSink) error
}

// Client implements kube.Client on top of an Execer.
type Client struct {
	ex Execer
}

// New returns a kubectl-backed kube.Client using ex to run commands.
func New(ex Execer) *Client { return &Client{ex: ex} }

func (c *Client) run(ctx context.Context, kc []byte, id string, args ...string) ([]byte, error) {
	res, err := c.ex.Run(ctx, kc, id, args)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("kubectl exited %d", res.Code)
		}
		return nil, fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), msg)
	}
	return res.Stdout, nil
}

func (c *Client) Namespaces(ctx context.Context, cl *domain.Cluster, kc []byte) ([]string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "namespaces", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawObj `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode namespaces: %w", err)
	}
	out2 := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		out2 = append(out2, it.Metadata.Name)
	}
	sort.Strings(out2)
	return out2, nil
}

func (c *Client) Workloads(ctx context.Context, cl *domain.Cluster, kc []byte, namespace string) ([]kube.WorkloadSummary, error) {
	args := []string{"get", "deployments,statefulsets,daemonsets,jobs,cronjobs"}
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
		Items []rawObj `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode workloads: %w", err)
	}
	res := make([]kube.WorkloadSummary, 0, len(list.Items))
	for _, it := range list.Items {
		k, ok := kindFromAPI(it.Kind)
		if !ok {
			continue
		}
		res = append(res, it.summary(k))
	}
	return res, nil
}

func (c *Client) Workload(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.WorkloadRef) (*kube.WorkloadDetail, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", ref.Kind.Resource(), ref.Name, "-n", ref.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var obj rawObj
	if err := json.Unmarshal(out, &obj); err != nil {
		return nil, fmt.Errorf("decode workload: %w", err)
	}
	detail := obj.detail(ref.Kind)
	// Pods are best-effort: a workload with no selector (or a transient list error) still renders.
	if sel := obj.Spec.Selector.MatchLabels; len(sel) > 0 {
		if pods, perr := c.pods(ctx, cl, kc, ref.Namespace, sel); perr == nil {
			detail.Pods = pods
		}
	}
	return &detail, nil
}

func (c *Client) pods(ctx context.Context, cl *domain.Cluster, kc []byte, namespace string, selector map[string]string) ([]kube.PodInfo, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "pods", "-n", namespace, "-l", labelSelector(selector), "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawPod `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode pods: %w", err)
	}
	pods := make([]kube.PodInfo, 0, len(list.Items))
	for _, p := range list.Items {
		pods = append(pods, p.info())
	}
	return pods, nil
}

func (c *Client) Manifest(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.WorkloadRef) (string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", ref.Kind.Resource(), ref.Name, "-n", ref.Namespace, "-o", "yaml")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *Client) Events(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.WorkloadRef) ([]kube.Event, error) {
	// One list of the namespace's events, filtered to the workload and its owned objects (pods and
	// replicasets are named "<workload>-…"), so pod-level events show alongside controller events -
	// a field-selector on involvedObject.name can't express that prefix match.
	out, err := c.run(ctx, kc, cl.ID, "get", "events", "-n", ref.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawEvent `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode events: %w", err)
	}
	var res []kube.Event
	for _, e := range list.Items {
		n := e.InvolvedObject.Name
		if n == ref.Name || strings.HasPrefix(n, ref.Name+"-") {
			res = append(res, e.event())
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].LastSeen.After(res[j].LastSeen) })
	if len(res) > 100 {
		res = res[:100]
	}
	return res, nil
}

func (c *Client) Scale(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.WorkloadRef, replicas int, readOnly bool) error {
	if readOnly {
		return fmt.Errorf("scaling is not permitted - you have read-only (view) access to this cluster")
	}
	if !ref.Kind.Scalable() {
		return fmt.Errorf("%s workloads cannot be scaled by replicas", ref.Kind)
	}
	if replicas < 0 {
		return fmt.Errorf("replicas must be >= 0")
	}
	_, err := c.run(ctx, kc, cl.ID,
		"scale", ref.Kind.Resource()+"/"+ref.Name, "-n", ref.Namespace,
		fmt.Sprintf("--replicas=%d", replicas))
	return err
}

func (c *Client) Logs(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.LogRef, sink kube.LogSink) error {
	tail := ref.TailLines
	if tail <= 0 || tail > 5000 {
		tail = 200
	}
	args := []string{"logs", "-n", ref.Namespace, ref.Pod, fmt.Sprintf("--tail=%d", tail)}
	if ref.Container != "" {
		args = append(args, "-c", ref.Container)
	}
	if ref.Follow {
		args = append(args, "-f")
	}
	return c.ex.Stream(ctx, kc, cl.ID, args, sink)
}

func labelSelector(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- kubectl -o json shapes ----------------------------------------------------

type rawObj struct {
	Kind     string    `json:"kind"`
	Metadata metaObj   `json:"metadata"`
	Spec     specObj   `json:"spec"`
	Status   statusObj `json:"status"`
}

type metaObj struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
}

type specObj struct {
	Replicas       *int        `json:"replicas"`
	Completions    *int        `json:"completions"`
	Parallelism    *int        `json:"parallelism"`
	Schedule       string      `json:"schedule"`
	Suspend        *bool       `json:"suspend"`
	Strategy       typeHolder  `json:"strategy"`
	UpdateStrategy typeHolder  `json:"updateStrategy"`
	Selector       selectorObj `json:"selector"`
	Template       podTemplate `json:"template"`
	JobTemplate    struct {
		Spec struct {
			Template podTemplate `json:"template"`
		} `json:"spec"`
	} `json:"jobTemplate"`
}

type typeHolder struct {
	Type string `json:"type"`
}

type selectorObj struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type podTemplate struct {
	Spec struct {
		Containers []containerObj `json:"containers"`
	} `json:"spec"`
}

type containerObj struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type statusObj struct {
	ReadyReplicas          int             `json:"readyReplicas"`
	UpdatedReplicas        int             `json:"updatedReplicas"`
	AvailableReplicas      int             `json:"availableReplicas"`
	DesiredNumberScheduled int             `json:"desiredNumberScheduled"`
	NumberReady            int             `json:"numberReady"`
	UpdatedNumberScheduled int             `json:"updatedNumberScheduled"`
	NumberAvailable        int             `json:"numberAvailable"`
	Succeeded              int             `json:"succeeded"`
	Failed                 int             `json:"failed"`
	Active                 json.RawMessage `json:"active"` // int (Job) or []objectRef (CronJob)
	Conditions             []condObj       `json:"conditions"`
}

type condObj struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
	LastUpdateTime     time.Time `json:"lastUpdateTime"`
}

// containers returns the pod-template containers, reading the CronJob's nested jobTemplate.
func (o rawObj) containers(k kube.WorkloadKind) []kube.Container {
	src := o.Spec.Template.Spec.Containers
	if k == kube.KindCronJob {
		src = o.Spec.JobTemplate.Spec.Template.Spec.Containers
	}
	out := make([]kube.Container, 0, len(src))
	for _, ct := range src {
		out = append(out, kube.Container{Name: ct.Name, Image: ct.Image})
	}
	return out
}

func (o rawObj) images(k kube.WorkloadKind) []string {
	cs := o.containers(k)
	out := make([]string, 0, len(cs))
	for _, ct := range cs {
		out = append(out, ct.Image)
	}
	return out
}

// readyDesired normalizes each kind's replica/scheduling status into a ready/desired pair.
func (o rawObj) readyDesired(k kube.WorkloadKind) (ready, desired int) {
	switch k {
	case kube.KindDaemonSet:
		return o.Status.NumberReady, o.Status.DesiredNumberScheduled
	case kube.KindJob:
		d := 1
		switch {
		case o.Spec.Completions != nil:
			d = *o.Spec.Completions
		case o.Spec.Parallelism != nil:
			d = *o.Spec.Parallelism
		}
		return o.Status.Succeeded, d
	case kube.KindCronJob:
		return activeCount(o.Status.Active), 0
	default: // Deployment, StatefulSet
		return o.Status.ReadyReplicas, deref(o.Spec.Replicas)
	}
}

func (o rawObj) status(k kube.WorkloadKind, ready, desired int) string {
	switch k {
	case kube.KindJob:
		if condTrue(o.Status.Conditions, "Complete") {
			return "Complete"
		}
		if condTrue(o.Status.Conditions, "Failed") {
			return "Failed"
		}
		return "Running"
	case kube.KindCronJob:
		if o.Spec.Suspend != nil && *o.Spec.Suspend {
			return "Suspended"
		}
		return "Scheduled"
	default:
		if desired == 0 {
			return "Scaled to zero"
		}
		if ready >= desired {
			return "Running"
		}
		return "Progressing"
	}
}

func (o rawObj) strategy(k kube.WorkloadKind) string {
	switch k {
	case kube.KindDeployment:
		return o.Spec.Strategy.Type
	case kube.KindStatefulSet, kube.KindDaemonSet:
		return o.Spec.UpdateStrategy.Type
	default:
		return ""
	}
}

func (o rawObj) summary(k kube.WorkloadKind) kube.WorkloadSummary {
	ready, desired := o.readyDesired(k)
	s := kube.WorkloadSummary{
		Kind:            k,
		Namespace:       o.Metadata.Namespace,
		Name:            o.Metadata.Name,
		ReadyReplicas:   ready,
		DesiredReplicas: desired,
		Status:          o.status(k, ready, desired),
		Images:          o.images(k),
		CreatedAt:       o.Metadata.CreationTimestamp,
	}
	if k == kube.KindCronJob {
		s.Schedule = o.Spec.Schedule
		s.Suspended = o.Spec.Suspend != nil && *o.Spec.Suspend
	}
	return s
}

func (o rawObj) detail(k kube.WorkloadKind) kube.WorkloadDetail {
	conds := make([]kube.Condition, 0, len(o.Status.Conditions))
	for _, cd := range o.Status.Conditions {
		conds = append(conds, kube.Condition{
			Type: cd.Type, Status: cd.Status, Reason: cd.Reason, Message: cd.Message,
			Updated: latest(cd.LastTransitionTime, cd.LastUpdateTime),
		})
	}
	return kube.WorkloadDetail{
		WorkloadSummary:   o.summary(k),
		UpdatedReplicas:   o.Status.UpdatedReplicas + o.Status.UpdatedNumberScheduled,
		AvailableReplicas: o.Status.AvailableReplicas + o.Status.NumberAvailable,
		Strategy:          o.strategy(k),
		Selector:          o.Spec.Selector.MatchLabels,
		Labels:            o.Metadata.Labels,
		Containers:        o.containers(k),
		Conditions:        conds,
		Pods:              []kube.PodInfo{},
	}
}

type rawPod struct {
	Metadata metaObj `json:"metadata"`
	Spec     struct {
		NodeName   string         `json:"nodeName"`
		Containers []containerObj `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase             string `json:"phase"`
		PodIP             string `json:"podIP"`
		ContainerStatuses []struct {
			Name         string `json:"name"`
			Ready        bool   `json:"ready"`
			RestartCount int    `json:"restartCount"`
			State        struct {
				Waiting *struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
				Terminated *struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (p rawPod) info() kube.PodInfo {
	total := len(p.Status.ContainerStatuses)
	if total == 0 {
		total = len(p.Spec.Containers)
	}
	ready, restarts := 0, 0
	status := p.Status.Phase
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
		// Surface a notable container reason (CrashLoopBackOff, ImagePullBackOff, …) over the phase.
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			status = cs.State.Waiting.Reason
		} else if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && p.Status.Phase != "Running" {
			status = cs.State.Terminated.Reason
		}
	}
	names := make([]string, 0, len(p.Spec.Containers))
	for _, ct := range p.Spec.Containers {
		names = append(names, ct.Name)
	}
	return kube.PodInfo{
		Name:       p.Metadata.Name,
		Ready:      fmt.Sprintf("%d/%d", ready, total),
		Status:     status,
		Restarts:   restarts,
		Node:       p.Spec.NodeName,
		IP:         p.Status.PodIP,
		CreatedAt:  p.Metadata.CreationTimestamp,
		Containers: names,
	}
}

type rawEvent struct {
	Type           string    `json:"type"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	Count          int       `json:"count"`
	LastTimestamp  time.Time `json:"lastTimestamp"`
	EventTime      time.Time `json:"eventTime"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
}

func (e rawEvent) event() kube.Event {
	count := e.Count
	if count == 0 {
		count = 1
	}
	obj := e.InvolvedObject.Name
	if e.InvolvedObject.Kind != "" {
		obj = e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
	}
	return kube.Event{
		Type: e.Type, Reason: e.Reason, Message: e.Message, Count: count,
		LastSeen: latest(e.LastTimestamp, e.EventTime), Object: obj,
	}
}

// ---- small helpers -------------------------------------------------------------

func kindFromAPI(kind string) (kube.WorkloadKind, bool) {
	switch kind {
	case "Deployment":
		return kube.KindDeployment, true
	case "StatefulSet":
		return kube.KindStatefulSet, true
	case "DaemonSet":
		return kube.KindDaemonSet, true
	case "Job":
		return kube.KindJob, true
	case "CronJob":
		return kube.KindCronJob, true
	default:
		return "", false
	}
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func condTrue(conds []condObj, typ string) bool {
	for _, c := range conds {
		if c.Type == typ && c.Status == "True" {
			return true
		}
	}
	return false
}

// activeCount reads a status.active that is an int (Job) or a []objectReference (CronJob).
func activeCount(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return len(arr)
	}
	return 0
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
