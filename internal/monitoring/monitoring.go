// Package monitoring is the request-driven cluster-observability seam behind the portal's
// Monitoring page: it runs curated PromQL against a Ready cluster's in-cluster Prometheus (the one
// kube-prometheus-stack installs) and returns typed panel results the portal renders as native
// gauges, stat tiles (with sparklines), line/area/stacked time-series, top-k bar lists,
// control-plane status grids, and active-alert lists.
//
// Like the metrics/health seams it is read-only, live/observed telemetry (never desired state); like
// the kube (Workloads) seam it is on-demand and interactive rather than sampled on the reconcile
// ticker. And like every seam that touches a cluster, only the worker can reach the cluster network
// (see docs/networking.md): the real implementation (internal/monitoring/promql) reuses the same
// kubectl exec path the Workloads seam uses - `kubectl get --raw .../services/proxy/api/v1/query` -
// so no new API↔worker transport is introduced. The Fake synthesizes plausible, drifting series from
// control-plane state so the whole page is demoable under `make up-fake`.
//
// The panel/PromQL registry (Tabs) is the single source both the Fake and the real querier read, so
// they always agree on which panels exist. We deliberately render a curated, load-bearing subset of
// what the stack ships - SLO, control plane, USE (compute), workloads, networking, alerts - leaning
// on kube-prometheus-stack's own recording & alerting rules, not every Grafana dashboard (see
// CLAUDE.md fidelity stance). Panels within a tab carry a Section (titled row-groups the portal
// renders headers for) and a Desc (an info tooltip), so the page explains itself.
//
// Fidelity/security shortcut: the query runs with the cluster ADMIN kubeconfig server-side, because
// it is read-only aggregate telemetry with no secret exposure (the built-in `view` role a read-role
// member holds can't `get services/proxy`). Access is still gated by the app's owner/group/admin
// view check. Production would mint a Prometheus-scoped read token instead of widening viewer RBAC.
package monitoring

import (
	"context"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// AddonName is the catalog add-on that provides this telemetry. The Monitoring page is gated on it
// being installed; a cluster without it exposes no monitoring.
const AddonName = "kube-prometheus-stack"

// Namespace / Prometheus service coordinates the real querier proxies through the API server. The
// release name is the add-on name, so kube-prometheus-stack's Prometheus Service is
// "<release>-prometheus" on port 9090 in the monitoring namespace.
const (
	Namespace         = "monitoring-system"
	PrometheusService = "kube-prometheus-stack-prometheus"
	PrometheusPort    = "9090"
)

// DefaultRange is the lookback window the Monitoring page opens with (and the fallback for an empty
// or unrecognised window selection).
const DefaultRange = "15m"

// Ranges are the selectable lookback windows for the Monitoring page's time-range picker, in
// ascending order. They govern the range (time-series) panels; instant panels (slo/gauge/stat/
// status/alerts) are point-in-time and ignore the selection.
var Ranges = []string{"5m", "15m", "30m", "1h", "3h", "12h"}

var rangeDurations = map[string]time.Duration{
	"5m":  5 * time.Minute,
	"15m": 15 * time.Minute,
	"30m": 30 * time.Minute,
	"1h":  time.Hour,
	"3h":  3 * time.Hour,
	"12h": 12 * time.Hour,
}

// ParseRange resolves a picker window ("5m"…"12h") to its lookback duration, falling back to
// DefaultRange for an empty or unrecognised value so a stale deep-link never errors the page.
func ParseRange(s string) time.Duration {
	if d, ok := rangeDurations[s]; ok {
		return d
	}
	return rangeDurations[DefaultRange]
}

// StepFor returns the query_range resolution for a window: ~60 points across the window, floored at
// 15s so a short window doesn't over-sample past the scrape interval. Shared by the real querier and
// the Fake so both render the same point density regardless of the chosen window.
func StepFor(window time.Duration) time.Duration {
	step := window / 60
	if step < 15*time.Second {
		step = 15 * time.Second
	}
	return step
}

// Enabled reports whether the cluster has the monitoring stack installed (and thus a Prometheus to
// query). Mirrors reconcile.metricsEnabled for the metrics-server add-on.
func Enabled(c *domain.Cluster) bool {
	for _, a := range c.Addons {
		if a.Name == AddonName && a.Phase == "installed" {
			return true
		}
	}
	return false
}

// PanelKind is how the portal renders a panel. The wire/JSON value is the lowercase form.
type PanelKind string

const (
	KindSLO        PanelKind = "slo"        // a big availability ratio (0..1) with an SLO target
	KindGauge      PanelKind = "gauge"      // a utilisation ratio (0..1) rendered as a gauge
	KindStat       PanelKind = "stat"       // a single scalar (count / bytes/s / …), Unit-formatted
	KindTimeSeries PanelKind = "timeseries" // one or more named series over the selected window
	KindBars       PanelKind = "bars"       // a top-k horizontal bar list (instant vector, sorted desc)
	KindStatus     PanelKind = "status"     // an up/down grid (one row per component/instance)
	KindAlerts     PanelKind = "alerts"     // active Prometheus alerts
)

// Viz values refine how a time-series panel draws its series; the zero value is a plain line chart.
const (
	VizLine    = ""        // default: 2px lines
	VizArea    = "area"    // line with a soft area wash - single-series throughput panels
	VizStacked = "stacked" // stacked area - part-to-whole composition (rate by code, CPU by namespace)
)

// Point is one (unix-seconds, value) sample in a time series.
type Point struct {
	T float64 `json:"t"`
	V float64 `json:"v"`
}

// Series is a single named line in a time-series panel (the name comes from a metric label, e.g. the
// verb, code, instance, or direction the panel legends on).
type Series struct {
	Name   string  `json:"name"`
	Points []Point `json:"points"`
}

// Bar is one row of a bars panel: a name and its magnitude (already sorted desc by the querier).
type Bar struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// StatusRow is one row of a status grid: a component and whether it is up, with a short detail.
type StatusRow struct {
	Label  string `json:"label"`
	Up     bool   `json:"up"`
	Detail string `json:"detail,omitempty"`
}

// Alert is one active Prometheus alert (from the stack's shipped alerting rules).
type Alert struct {
	Name     string    `json:"name"`
	Severity string    `json:"severity"` // critical | warning | info | none
	State    string    `json:"state"`    // firing | pending
	Summary  string    `json:"summary"`  // annotations.summary/description
	ActiveAt time.Time `json:"active_at,omitzero"`
}

// PanelResult is one resolved panel. Exactly one of Value/Series/Rows/Alerts is populated per Kind
// (Value for slo/gauge/stat). A per-panel query failure is soft: Error is set and the other panels
// on the tab still render, so one broken metric doesn't blank the page.
type PanelResult struct {
	ID     string      `json:"id"`
	Title  string      `json:"title"`
	Unit   string      `json:"unit"` // ratio | count | cores | rps | ops | pps | bytes | Bps | ms | s
	Kind   PanelKind   `json:"kind"`
	Value  *float64    `json:"value,omitempty"`  // slo | gauge (0..1 ratio) | stat (raw)
	Target *float64    `json:"target,omitempty"` // slo target (0..1), for the SLO panel
	Series []Series    `json:"series,omitempty"` // timeseries; also a stat's sparkline when its spec is Range
	Bars   []Bar       `json:"bars,omitempty"`
	Rows   []StatusRow `json:"rows,omitempty"`
	Alerts []Alert     `json:"alerts,omitempty"`
	Error  string      `json:"error,omitempty"`
	// Layout/presentation hints copied from the spec - orthogonal to Kind.
	Section  string `json:"section,omitempty"` // titled group the panel renders under on its tab
	Desc     string `json:"desc,omitempty"`    // one-line "what am I looking at" shown as an info tooltip
	Viz      string `json:"viz,omitempty"`     // timeseries only: VizLine | VizArea | VizStacked
	Featured bool   `json:"featured,omitempty"`
}

// TabData is a resolved tab: every panel in it, plus when it was generated.
type TabData struct {
	Tab         string        `json:"tab"`
	GeneratedAt time.Time     `json:"generated_at"`
	Panels      []PanelResult `json:"panels"`
}

// TabMeta is the lightweight tab descriptor the portal uses to build its tab bar.
type TabMeta struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// PanelSpec declares one panel and the PromQL that backs it. Legend names the metric label a
// time-series/bars/status panel splits on (e.g. "code", "verb", "namespace", "job"); a
// comma-separated list ("namespace,pod") joins the labels with "/"; empty means a single unnamed
// series/row. Range selects the range query (query_range → a matrix) vs an instant query - on a
// KindStat panel it means "sparkline stat": the series is returned alongside the latest value.
type PanelSpec struct {
	ID       string
	Title    string
	Unit     string
	Kind     PanelKind
	Query    string
	Legend   string
	Range    bool
	Target   float64 // slo panels only
	Section  string  // titled group the panel renders under (consecutive panels share a header)
	Desc     string  // one-line explanation surfaced as an info tooltip on the panel
	Viz      string  // timeseries only: VizLine | VizArea | VizStacked
	Featured bool    // standout treatment for the kind: hero ring for a gauge, full-width for a timeseries
}

// TabSpec is a named tab and its ordered panels.
type TabSpec struct {
	ID     string
	Title  string
	Panels []PanelSpec
}

// LegendName resolves a display name from a panel's Legend against one result's label set: each
// comma-separated label is looked up and the non-empty values joined with "/" ("namespace,pod" →
// "kube-system/coredns-…"). Empty legend, or none of the labels present, yields "". Shared by the
// real querier and the Fake so both name series/bars identically.
func LegendName(legend string, labels map[string]string) string {
	if legend == "" {
		return ""
	}
	name := ""
	for _, l := range strings.Split(legend, ",") {
		if v := labels[l]; v != "" {
			if name != "" {
				name += "/"
			}
			name += v
		}
	}
	return name
}

// fsFilter selects real filesystems (skip pseudo/bind mounts), matching the node-exporter
// dashboards' `fstype!="", mountpoint!=""`.
const fsFilter = `fstype!="",mountpoint!=""`

// byNode wraps a PromQL expression whose result is legended by the raw "instance" label
// (node-exporter, etcd, the scheduler - anything scraped directly off a node's own address) with a
// join against kube-state-metrics' kube_node_info, attaching the human-readable Kubernetes node name
// as a "node" label. Both hostNetwork node-exporter and kubeadm's control-plane static pods scrape on
// the node's own IP (just different ports), and kube_node_info carries that same IP as `internal_ip`
// - so joining on the bare IP (stripping the port from both sides) works regardless of which
// component's port `instance` happens to be. Panels built with this legend on "node", not "instance",
// so the portal shows "worker-2", never an IP:port.
func byNode(expr string) string {
	// PromQL string literals use Go-style backslash escaping, so the regex's own "\d" must be
	// written "\\d" here - a bare "\d" is not a recognized escape and Prometheus rejects the whole
	// query with a parse error before it ever runs.
	return `label_replace(` + expr + `, "node_ip", "$1", "instance", "(.+):\\d+")` +
		` * on (node_ip) group_left(node) label_replace(kube_node_info, "node_ip", "$1", "internal_ip", "(.+)")`
}

// Tabs is the panel/PromQL registry - the single source the Fake and the real querier both read.
// Every expression is taken from kube-prometheus-stack's own recording rules and Grafana dashboards
// (kubernetes-mixin / node-exporter-mixin / etcd-mixin) rather than hand-rolled, so the numbers match
// what a cluster operator sees in Grafana:
//   - apiserver: apiserver_request:availability30d, code_resource:apiserver_request_total:rate5m,
//     cluster_quantile:apiserver_request_sli_duration_seconds:histogram_quantile (SLO/burn-rate rules)
//   - nodes (USE): the node-exporter instance:* recording rules the node-cluster-rsrc-use dashboard uses
//   - cluster CPU/mem: node.rules cluster:node_cpu:ratio_rate5m and :node_memory_MemAvailable_bytes:sum
//   - etcd / scheduler / controller-manager / kube-proxy / coredns: their dashboards' exact queries
var Tabs = []TabSpec{
	{
		ID: "overview", Title: "Overview",
		Panels: []PanelSpec{
			// -- Cluster at a glance: the two headline gauges plus the fleet-level counters. --
			{ID: "cpu_util", Title: "Cluster CPU utilisation", Unit: "ratio", Kind: KindGauge, Featured: true,
				Section: "Cluster at a glance",
				Desc:    "Share of all node CPU busy right now, averaged over 5m (node.rules recording rule).",
				Query:   `cluster:node_cpu:ratio_rate5m`},
			{ID: "mem_util", Title: "Cluster memory utilisation", Unit: "ratio", Kind: KindGauge, Featured: true,
				Section: "Cluster at a glance",
				Desc:    "Memory in use across all nodes: 1 − available/total.",
				Query:   `1 - sum(:node_memory_MemAvailable_bytes:sum) / sum(node_memory_MemTotal_bytes)`},
			{ID: "nodes_ready", Title: "Nodes Ready", Unit: "count", Kind: KindStat,
				Section: "Cluster at a glance",
				Desc:    "Nodes whose kubelet reports the Ready condition true.",
				Query:   `sum(kube_node_status_condition{condition="Ready",status="true"})`},
			{ID: "pods_running", Title: "Running pods", Unit: "count", Kind: KindStat,
				Section: "Cluster at a glance",
				Query:   `sum(kube_pod_status_phase{phase="Running"})`},
			{ID: "pods_pending", Title: "Pending pods", Unit: "count", Kind: KindStat,
				Section: "Cluster at a glance",
				Desc:    "Pods waiting to be scheduled or pulled - anything above 0 for long deserves a look.",
				Query:   `sum(kube_pod_status_phase{phase="Pending"})`},
			{ID: "restarts_1h", Title: "Container restarts (1h)", Unit: "count", Kind: KindStat,
				Section: "Cluster at a glance",
				Desc:    "Container restarts across the whole cluster in the last hour - crash-loops show up here first.",
				Query:   `sum(increase(kube_pod_container_status_restarts_total[1h]))`},

			// -- Capacity & headroom: how booked the cluster is, not just how busy. Commitments are
			// requests vs allocatable (the kubernetes-mixin cluster dashboard's expressions) and can
			// legitimately exceed 100% on an overcommitted cluster. --
			{ID: "cpu_commit", Title: "CPU committed", Unit: "ratio", Kind: KindGauge,
				Section: "Capacity & headroom",
				Desc:    "Sum of pod CPU requests vs allocatable CPU. Above 100% means the cluster is overcommitted.",
				Query:   `sum(namespace_cpu:kube_pod_container_resource_requests:sum) / sum(kube_node_status_allocatable{resource="cpu"})`},
			{ID: "mem_commit", Title: "Memory committed", Unit: "ratio", Kind: KindGauge,
				Section: "Capacity & headroom",
				Desc:    "Sum of pod memory requests vs allocatable memory. Above 100% means overcommit.",
				Query:   `sum(namespace_memory:kube_pod_container_resource_requests:sum) / sum(kube_node_status_allocatable{resource="memory"})`},
			{ID: "disk_util", Title: "Cluster filesystem used", Unit: "ratio", Kind: KindGauge,
				Section: "Capacity & headroom",
				Desc:    "Used space across every real filesystem on every node.",
				Query:   `sum(max without (fstype, mountpoint) (node_filesystem_size_bytes{` + fsFilter + `} - node_filesystem_avail_bytes{` + fsFilter + `})) / sum(max without (fstype, mountpoint) (node_filesystem_size_bytes{` + fsFilter + `}))`},
			// A sparkline stat (KindStat + Range): the latest value with the window's trend behind it.
			{ID: "net_throughput", Title: "Cluster network I/O", Unit: "Bps", Kind: KindStat, Range: true,
				Section: "Capacity & headroom",
				Desc:    "Receive + transmit across all nodes (loopback excluded), with the selected window's trend.",
				Query:   `sum(instance:node_network_receive_bytes_excluding_lo:rate5m) + sum(instance:node_network_transmit_bytes_excluding_lo:rate5m)`},

			// -- API server service level: the 30d availability SLO (kube-apiserver-slos: 99%
			// objective), its read/write split, and the request-rate/latency pair. --
			{ID: "apiserver_slo", Title: "API server availability (30d SLO)", Unit: "ratio", Kind: KindSLO, Target: 0.99,
				Section: "API server service level",
				Desc:    "Fraction of API requests served successfully and in time over the last 30 days, against the stack's 99% objective.",
				Query:   `apiserver_request:availability30d{verb="all"}`},
			{ID: "apiserver_avail_read", Title: "Read availability (30d)", Unit: "ratio", Kind: KindStat,
				Section: "API server service level",
				Query:   `apiserver_request:availability30d{verb="read"}`},
			{ID: "apiserver_avail_write", Title: "Write availability (30d)", Unit: "ratio", Kind: KindStat,
				Section: "API server service level",
				Query:   `apiserver_request:availability30d{verb="write"}`},
			{ID: "apiserver_rate", Title: "Request rate by response code", Unit: "rps", Kind: KindTimeSeries, Range: true, Legend: "code", Viz: VizStacked,
				Section: "API server service level",
				Desc:    "API server requests per second, stacked by HTTP response code family.",
				Query:   `sum by (code) (code_resource:apiserver_request_total:rate5m)`},
			{ID: "apiserver_latency", Title: "p99 latency (read/write)", Unit: "s", Kind: KindTimeSeries, Range: true, Legend: "verb",
				Section: "API server service level",
				Desc:    "99th-percentile SLI request duration, split by read vs write verbs.",
				// max by (verb): unlike apiserver_rate's code_resource:*, this recording rule doesn't
				// collapse per-apiserver-instance series on its own, so an HA control plane (3
				// kube-apiserver replicas) would otherwise return 3 near-identical "read" series and 3
				// "write" series - same legend name repeated, only the last-received one actually
				// plotted. Collapsing to the slowest instance per verb keeps the legend to exactly two
				// entries and surfaces the worst-case replica rather than an arbitrary one.
				Query: `max by (verb) (cluster_quantile:apiserver_request_sli_duration_seconds:histogram_quantile{quantile="0.99"})`},
		},
	},
	{
		ID: "controlplane", Title: "Control plane",
		Panels: []PanelSpec{
			{ID: "components_up", Title: "Component health", Unit: "", Kind: KindStatus, Legend: "job",
				Section: "Component health",
				Desc:    "Scrape-level up/down for every control-plane component; a multi-instance component is Up only when all its instances are.",
				Query:   `up{job=~"apiserver|kube-apiserver|kube-controller-manager|kube-scheduler|kube-proxy|coredns|kube-dns|kubelet|kube-etcd|etcd"}`},

			// API-server error ratio (5xx) by verb + in-flight load (apiserver dashboard).
			{ID: "apiserver_errors", Title: "API server error ratio (5xx)", Unit: "ratio", Kind: KindTimeSeries, Range: true, Legend: "verb",
				Section: "API server",
				Desc:    "Share of requests answered 5xx, by verb - the error-budget burn signal.",
				Query:   `sum by (verb) (code_resource:apiserver_request_total:rate5m{code=~"5.."}) / sum by (verb) (code_resource:apiserver_request_total:rate5m)`},
			{ID: "apiserver_inflight", Title: "In-flight requests", Unit: "count", Kind: KindTimeSeries, Range: true, Legend: "request_kind", Viz: VizStacked,
				Section: "API server",
				Desc:    "Requests currently being served, stacked by read-only vs mutating - sustained growth means the server is saturating.",
				Query:   `sum by (request_kind) (apiserver_current_inflight_requests)`},

			// etcd (etcd-mixin dashboard): DB size + disk fsync/commit p99, by node (byNode joins in
			// the human-readable node name - the raw series only carry etcd's own instance IP:port).
			{ID: "etcd_db_size", Title: "etcd database size", Unit: "bytes", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "etcd",
				Query:   byNode(`etcd_mvcc_db_total_size_in_bytes{job=~".*etcd.*"}`)},
			{ID: "etcd_db_in_use", Title: "etcd database size in use", Unit: "bytes", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "etcd",
				Desc: "The LOGICALLY used part of the backend store. The gap between this and the total size " +
					"above is fragmentation - space compaction has freed but which only defragmentation returns " +
					"to the filesystem. The platform defragments automatically when that gap gets large enough " +
					"(see the cluster's etcd backend store health check).",
				Query: byNode(`etcd_mvcc_db_total_size_in_use_in_bytes{job=~".*etcd.*"}`)},
			{ID: "etcd_quota_usage", Title: "etcd quota used", Unit: "percent", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "etcd",
				Desc: "Backend store as a share of --quota-backend-bytes. Reaching 100% arms etcd's NOSPACE " +
					"alarm and makes the WHOLE cluster read-only until it is defragmented and the alarm disarmed.",
				Query: byNode(`100 * etcd_mvcc_db_total_size_in_bytes{job=~".*etcd.*"} / on (instance) etcd_server_quota_backend_bytes{job=~".*etcd.*"}`)},
			{ID: "etcd_fsync_p99", Title: "etcd WAL fsync p99", Unit: "s", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "etcd",
				Desc:    "Disk write-ahead-log fsync latency - the classic \"etcd is on slow disk\" signal (should stay under ~10ms).",
				Query:   byNode(`histogram_quantile(0.99, sum(rate(etcd_disk_wal_fsync_duration_seconds_bucket{job=~".*etcd.*"}[5m])) by (instance, le))`)},
			{ID: "etcd_commit_p99", Title: "etcd backend commit p99", Unit: "s", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "etcd",
				Query:   byNode(`histogram_quantile(0.99, sum(rate(etcd_disk_backend_commit_duration_seconds_bucket{job=~".*etcd.*"}[5m])) by (instance, le))`)},

			// Scheduler (kube-scheduler.rules + scheduler dashboard; the shipped recording rule
			// aggregates `without(instance, pod)` - genuinely cluster-wide) and controller-manager
			// (its dashboard's work rate + workqueue p99).
			{ID: "scheduler_latency_p99", Title: "Scheduler attempt p99 latency", Unit: "s", Kind: KindTimeSeries, Range: true,
				Section: "Scheduler & controller manager",
				Query:   `cluster_quantile:scheduler_scheduling_attempt_duration_seconds:histogram_quantile{quantile="0.99"}`},
			{ID: "scheduler_attempts", Title: "Scheduling attempts", Unit: "rps", Kind: KindTimeSeries, Range: true, Legend: "result",
				Section: "Scheduler & controller manager",
				Desc:    "Pod scheduling attempts per second by outcome - a growing \"unschedulable\" line means pods can't fit.",
				Query:   `sum by (result) (rate(scheduler_schedule_attempts_total[5m]))`},
			{ID: "controller_workqueue", Title: "Controller work rate", Unit: "rps", Kind: KindTimeSeries, Range: true, Legend: "name",
				Section: "Scheduler & controller manager",
				Desc:    "Items added per second to the busiest controller workqueues.",
				Query:   `topk(6, sum by (name) (rate(workqueue_adds_total{job=~".*controller-manager.*"}[5m])))`},
			{ID: "controller_wq_latency", Title: "Controller workqueue p99 latency", Unit: "s", Kind: KindTimeSeries, Range: true, Legend: "",
				Section: "Scheduler & controller manager",
				Desc:    "How long items wait in controller workqueues before being processed.",
				Query:   `histogram_quantile(0.99, sum(rate(workqueue_queue_duration_seconds_bucket{job=~".*controller-manager.*"}[5m])) by (le))`},

			// Per-node agents + DNS. Kubelet PLEG comes from kubelet.rules, which already joins in a
			// "node" label itself (group_left against kubelet_node_name) - no byNode() wrap needed,
			// unlike the etcd panels above.
			{ID: "kubelet_pleg_p99", Title: "Kubelet PLEG relist p99", Unit: "s", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "Node agents & DNS",
				Desc:    "Pod-lifecycle-event relist latency per kubelet - the classic node-health signal (should stay well under 1s).",
				Query:   `node_quantile:kubelet_pleg_relist_duration_seconds:histogram_quantile{quantile="0.99"}`},
			{ID: "kubeproxy_sync_p99", Title: "kube-proxy rule sync p99", Unit: "s", Kind: KindTimeSeries, Range: true, Legend: "",
				Section: "Node agents & DNS",
				Query:   `histogram_quantile(0.99, sum(rate(kubeproxy_sync_proxy_rules_duration_seconds_bucket[5m])) by (le))`},
			{ID: "coredns_rate", Title: "CoreDNS request rate", Unit: "rps", Kind: KindTimeSeries, Range: true, Legend: "proto",
				Section: "Node agents & DNS",
				Query:   `sum by (proto) (rate(coredns_dns_requests_total[5m]))`},
			{ID: "coredns_latency_p99", Title: "CoreDNS p99 latency", Unit: "s", Kind: KindTimeSeries, Range: true, Legend: "",
				Section: "Node agents & DNS",
				Query:   `histogram_quantile(0.99, sum(rate(coredns_dns_request_duration_seconds_bucket[5m])) by (le))`},
		},
	},
	{
		ID: "compute", Title: "Compute (USE)",
		// Utilisation + saturation per resource, sectioned CPU / memory / disk / network - the USE
		// method as the node-cluster-rsrc-use / node-rsrc-use dashboards graph it, using the same
		// node-exporter recording rules so the numbers match Grafana exactly. Per-node series legend
		// on "node" (byNode() joins in the human-readable Kubernetes node name - the raw
		// node-exporter series only carry the scrape instance, an IP:port).
		Panels: []PanelSpec{
			{ID: "cpu_by_node", Title: "CPU utilisation by node", Unit: "ratio", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "CPU",
				Query:   byNode(`instance:node_cpu_utilisation:rate5m{job="node-exporter"}`)},
			{ID: "load_by_node", Title: "CPU saturation (load1 per core)", Unit: "ratio", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "CPU",
				Desc:    "1-minute load average divided by core count - above 1.0 means runnable work is queueing.",
				Query:   byNode(`instance:node_load1_per_cpu:ratio{job="node-exporter"}`)},
			{ID: "mem_by_node", Title: "Memory utilisation by node", Unit: "ratio", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "Memory",
				Query:   byNode(`instance:node_memory_utilisation:ratio{job="node-exporter"}`)},
			{ID: "memsat_by_node", Title: "Memory saturation (major page faults)", Unit: "ops", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "Memory",
				Desc:    "Major page faults per second - sustained faults mean the node is memory-starved and hitting disk.",
				Query:   byNode(`instance:node_vmstat_pgmajfault:rate5m{job="node-exporter"}`)},
			{ID: "disk_sat_by_node", Title: "Disk I/O saturation by node", Unit: "ratio", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "Disk & filesystem",
				Desc:    "Weighted time spent doing I/O - near 1.0 the device is the bottleneck.",
				Query:   byNode(`sum by (instance) (instance_device:node_disk_io_time_weighted_seconds:rate5m{job="node-exporter"})`)},
			{ID: "disk_iops_by_node", Title: "Disk IOPS by node", Unit: "ops", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "Disk & filesystem",
				Query:   byNode(`sum by (instance) (rate(node_disk_reads_completed_total{job="node-exporter"}[5m]) + rate(node_disk_writes_completed_total{job="node-exporter"}[5m]))`)},
			{ID: "disk_thru_by_node", Title: "Disk throughput by node", Unit: "Bps", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "Disk & filesystem",
				Query:   byNode(`sum by (instance) (rate(node_disk_read_bytes_total{job="node-exporter"}[5m]) + rate(node_disk_written_bytes_total{job="node-exporter"}[5m]))`)},
			{ID: "fs_by_node", Title: "Root filesystem used by node", Unit: "ratio", Kind: KindTimeSeries, Range: true, Legend: "node",
				Section: "Disk & filesystem",
				Query:   byNode(`max by (instance) (1 - node_filesystem_avail_bytes{fstype!="",mountpoint="/"} / node_filesystem_size_bytes{fstype!="",mountpoint="/"})`)},
			{ID: "net_by_dir", Title: "Network throughput", Unit: "Bps", Kind: KindTimeSeries, Range: true, Legend: "direction", Viz: VizArea,
				Section: "Network",
				Query:   `label_replace(sum(instance:node_network_receive_bytes_excluding_lo:rate5m), "direction", "receive", "", "") or label_replace(sum(instance:node_network_transmit_bytes_excluding_lo:rate5m), "direction", "transmit", "", "")`},
			{ID: "net_drops", Title: "Network saturation (dropped packets)", Unit: "pps", Kind: KindTimeSeries, Range: true, Legend: "direction",
				Section: "Network",
				Desc:    "Packets dropped at the node NICs (loopback excluded) - the network's saturation signal.",
				Query:   `label_replace(sum(instance:node_network_receive_drop_excluding_lo:rate5m), "direction", "receive", "", "") or label_replace(sum(instance:node_network_transmit_drop_excluding_lo:rate5m), "direction", "transmit", "", "")`},
		},
	},
	{
		ID: "workloads", Title: "Workloads",
		// What is actually consuming the cluster, from kube-state-metrics + the kubernetes-mixin
		// namespace recording rules - the tenant-facing complement to the node-centric Compute tab.
		Panels: []PanelSpec{
			{ID: "ns_cpu_bars", Title: "CPU usage by namespace", Unit: "cores", Kind: KindBars, Legend: "namespace",
				Section: "Namespace usage",
				Desc:    "Top namespaces by CPU actually used right now (not requested).",
				Query:   `topk(8, sum by (namespace) (node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate))`},
			{ID: "ns_mem_bars", Title: "Memory usage by namespace", Unit: "bytes", Kind: KindBars, Legend: "namespace",
				Section: "Namespace usage",
				Desc:    "Top namespaces by container working-set memory.",
				Query:   `topk(8, sum by (namespace) (container_memory_working_set_bytes{container!="",image!=""}))`},
			{ID: "ns_cpu_ts", Title: "CPU usage by namespace over time", Unit: "cores", Kind: KindTimeSeries, Range: true, Legend: "namespace", Viz: VizStacked, Featured: true,
				Section: "Namespace usage",
				Desc:    "The same per-namespace CPU, stacked over the selected window - composition and total in one picture.",
				Query:   `sum by (namespace) (node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate)`},
			{ID: "pods_not_ready", Title: "Pods not Ready", Unit: "count", Kind: KindStat,
				Section: "Pod & workload health",
				Desc:    "Pods failing their readiness condition - traffic is not being routed to them.",
				Query:   `sum(kube_pod_status_ready{condition="false"})`},
			{ID: "deploy_unavailable", Title: "Unavailable replicas", Unit: "count", Kind: KindStat,
				Section: "Pod & workload health",
				Desc:    "Deployment replicas currently unavailable across all namespaces.",
				Query:   `sum(kube_deployment_status_replicas_unavailable)`},
			{ID: "restart_pods", Title: "Restarts by pod (24h)", Unit: "count", Kind: KindBars, Legend: "namespace,pod",
				Section: "Pod & workload health",
				Desc:    "Pods with container restarts in the last 24 hours - empty is the healthy state.",
				Query:   `topk(8, sum by (namespace, pod) (increase(kube_pod_container_status_restarts_total[24h])) > 0)`},
			{ID: "pods_by_phase", Title: "Pods by phase", Unit: "count", Kind: KindTimeSeries, Range: true, Legend: "phase", Viz: VizStacked, Featured: true,
				Section: "Pod & workload health",
				Query:   `sum by (phase) (kube_pod_status_phase{phase=~"Running|Pending|Failed|Succeeded"})`},
		},
	},
	{
		ID: "networking", Title: "Networking",
		Panels: []PanelSpec{
			{ID: "cilium_up", Title: "Cilium health", Unit: "", Kind: KindStatus, Legend: "job",
				Section: "Cilium",
				Query:   `up{job=~".*cilium.*"}`},
			{ID: "cilium_endpoints", Title: "Managed endpoints", Unit: "count", Kind: KindStat,
				Section: "Cilium",
				Desc:    "Pods (endpoints) whose networking Cilium currently manages.",
				Query:   `sum(cilium_endpoint)`},
			{ID: "cilium_map_pressure", Title: "Max BPF map pressure", Unit: "ratio", Kind: KindGauge,
				Section: "Cilium",
				Desc:    "Fullest BPF map as a share of its capacity - a full map starts dropping new entries.",
				Query:   `max(cilium_bpf_map_pressure)`},
			{ID: "cilium_forward", Title: "Cilium forwarded packets", Unit: "pps", Kind: KindTimeSeries, Range: true, Legend: "", Viz: VizArea,
				Section: "Cilium",
				Query:   `sum(rate(cilium_forward_count_total[5m]))`},
			{ID: "cilium_drops", Title: "Cilium dropped packets by reason", Unit: "pps", Kind: KindTimeSeries, Range: true, Legend: "reason",
				Section: "Cilium",
				Desc:    "Packets Cilium dropped, by drop reason - \"Policy denied\" is a network policy doing its job.",
				Query:   `sum by (reason) (rate(cilium_drop_count_total[5m]))`},
			// Cluster pod network + TCP retransmit ratio (cluster-total dashboard).
			{ID: "pod_net", Title: "Pod network throughput", Unit: "Bps", Kind: KindTimeSeries, Range: true, Legend: "direction", Viz: VizArea,
				Section: "Cluster traffic",
				Query:   `label_replace(sum(rate(container_network_receive_bytes_total{namespace!=""}[5m])), "direction", "receive", "", "") or label_replace(sum(rate(container_network_transmit_bytes_total{namespace!=""}[5m])), "direction", "transmit", "", "")`},
			{ID: "tcp_retrans", Title: "TCP retransmit ratio", Unit: "ratio", Kind: KindTimeSeries, Range: true, Legend: "",
				Section: "Cluster traffic",
				Desc:    "Retransmitted vs sent TCP segments across the fleet - a rising ratio points at a lossy or congested path.",
				Query:   `sum(rate(node_netstat_Tcp_RetransSegs[5m])) / sum(rate(node_netstat_Tcp_OutSegs[5m]))`},
		},
	},
	{
		ID: "alerts", Title: "Alerts",
		Panels: []PanelSpec{
			{ID: "active_alerts", Title: "Active alerts", Unit: "", Kind: KindAlerts,
				Desc:  "Everything Prometheus is currently firing or evaluating, from the stack's shipped alerting rules. The Watchdog always fires by design.",
				Query: ``}, // resolved via the Prometheus /alerts endpoint, not /query
		},
	},
}

// TabMetas is the tab bar descriptor (id + title), derived from Tabs.
func TabMetas() []TabMeta {
	out := make([]TabMeta, 0, len(Tabs))
	for _, t := range Tabs {
		out = append(out, TabMeta{ID: t.ID, Title: t.Title})
	}
	return out
}

// tab looks up a tab spec by id.
func tab(id string) (TabSpec, bool) {
	for _, t := range Tabs {
		if t.ID == id {
			return t, true
		}
	}
	return TabSpec{}, false
}

// ErrUnknownTab is returned by a Querier for a tab id not in the registry.
var ErrUnknownTab = errNotFound("monitoring: unknown tab")

type errNotFound string

func (e errNotFound) Error() string { return string(e) }

// Querier resolves a Monitoring tab's panels for a Ready cluster given its admin kubeconfig. The
// window is the lookback for the tab's range panels (see ParseRange/StepFor); instant panels ignore
// it. Every call is per-request; a transient failure is a normal error the API surfaces, never fatal.
// Per-panel query failures are carried in PanelResult.Error rather than failing the whole tab.
type Querier interface {
	Tab(ctx context.Context, c *domain.Cluster, kubeconfig []byte, tab string, window time.Duration) (*TabData, error)
}
