package monitoring

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Fake is the in-process Querier used in fake mode (make up-fake): it synthesizes plausible,
// slowly-drifting panel results from a cluster's control-plane state, so the whole Monitoring page -
// SLO, gauges, control-plane status, USE time-series, Cilium, and the always-on Watchdog alert -
// renders with no real Prometheus, mirroring every other fake seam. Deterministic in the cluster id
// + panel id (stable baselines) plus a time term (drift).
type Fake struct{}

// NewFake returns the fake monitoring querier.
func NewFake() *Fake { return &Fake{} }

func (Fake) Tab(_ context.Context, c *domain.Cluster, _ []byte, tabID string, window time.Duration) (*TabData, error) {
	spec, ok := tab(tabID)
	if !ok {
		return nil, ErrUnknownTab
	}
	out := &TabData{Tab: tabID, GeneratedAt: time.Now(), Panels: make([]PanelResult, 0, len(spec.Panels))}
	for _, p := range spec.Panels {
		out.Panels = append(out.Panels, fakePanel(c, p, window))
	}
	return out, nil
}

func fakePanel(c *domain.Cluster, p PanelSpec, window time.Duration) PanelResult {
	r := PanelResult{ID: p.ID, Title: p.Title, Unit: p.Unit, Kind: p.Kind,
		Section: p.Section, Desc: p.Desc, Viz: p.Viz, Featured: p.Featured}
	seed := c.ID + "|" + p.ID
	switch p.Kind {
	case KindSLO:
		v := clamp(0.9995-0.0004*osc(seed, 1.0/900), 0.99, 0.99999)
		r.Value, r.Target = &v, ptr(p.Target)
	case KindGauge:
		base := map[string]float64{
			"cpu_util": 0.32, "mem_util": 0.48, "disk_util": 0.38, "cilium_map_pressure": 0.08,
			"cpu_commit": 0.56, "mem_commit": 0.63,
		}[p.ID]
		if base == 0 {
			base = 0.4
		}
		v := clamp(base+0.12*osc(seed, 1.0/300), 0.02, 0.97)
		r.Value = &v
	case KindStat:
		if p.Range { // sparkline stat: a series over the window, headlined by its newest sample
			r.Series = fakeSeries(c, p, seed, window)
			last := r.Series[0].Points[len(r.Series[0].Points)-1].V
			r.Value = &last
			return r
		}
		v := fakeStat(c, p.ID, seed)
		r.Value = &v
	case KindTimeSeries:
		r.Series = fakeSeries(c, p, seed, window)
	case KindBars:
		r.Bars = fakeBars(c, p, seed)
	case KindStatus:
		r.Rows = fakeStatus(c, p.ID)
	case KindAlerts:
		r.Alerts = fakeAlerts(seed)
	}
	return r
}

func fakeStat(c *domain.Cluster, id, seed string) float64 {
	nodes := float64(max(len(c.Nodes), 1))
	switch id {
	case "apiserver_avail_read":
		return clamp(0.99955-0.0004*osc(seed, 1.0/900), 0.99, 0.99999)
	case "apiserver_avail_write":
		return clamp(0.99975-0.0003*osc(seed, 1.0/900), 0.99, 0.99999)
	case "nodes_ready":
		return float64(len(c.Nodes))
	case "pods_running":
		return math.Round(8*nodes + 4*osc(seed, 1.0/600) + 6)
	case "pods_pending", "pods_not_ready":
		if osc(seed, 1.0/120) > 0.85 {
			return 1
		}
		return 0
	case "restarts_1h":
		if osc(seed, 1.0/900) > 0.7 {
			return 1
		}
		return 0
	case "deploy_unavailable":
		return 0
	case "cilium_endpoints":
		return math.Round(9*nodes + 5)
	default:
		return math.Round(10 + 5*osc(seed, 1.0/300))
	}
}

// fakeNamespaces is the plausible namespace set the workloads panels rank - what a real cluster
// from this platform actually runs (system components, the monitoring stack, Trivy, user default),
// heaviest first.
var fakeNamespaces = []struct {
	name       string
	cpu, bytes float64
}{
	{"monitoring-system", 0.24, 1.4e9}, // Prometheus is always the hungriest tenant
	{"kube-system", 0.31, 850e6},
	{"trivy-system", 0.07, 320e6},
	{"default", 0.05, 190e6},
	{"kube-public", 0.004, 12e6},
}

// fakeBars synthesizes a top-k bar list, largest first, drifting slightly so refreshes feel live.
func fakeBars(c *domain.Cluster, p PanelSpec, seed string) []Bar {
	if p.ID == "restart_pods" {
		// One habitual offender, plus a second pod that comes and goes - empty would also be
		// truthful, but the demo reads better with the panel populated.
		bars := []Bar{{Name: "default/demo-app-" + shortHash(seed), Value: math.Round(2 + 1.5*osc(seed, 1.0/1700))}}
		if osc(seed+"|2", 1.0/2300) > 0.3 {
			bars = append(bars, Bar{Name: "kube-system/coredns-" + shortHash(seed+"|2"), Value: 1})
		}
		return bars
	}
	nodes := float64(max(len(c.Nodes), 1))
	out := make([]Bar, 0, len(fakeNamespaces))
	for _, ns := range fakeNamespaces {
		base := ns.cpu
		if p.Unit == "bytes" {
			base = ns.bytes
		}
		v := base * (1 + 0.25*osc(seed+"|"+ns.name, 1.0/400)) * (0.7 + 0.3*nodes/3)
		out = append(out, Bar{Name: ns.name, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out
}

// shortHash yields a stable pod-name-ish suffix from a seed.
func shortHash(seed string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	return fmt.Sprintf("%05x", h.Sum32()%0xfffff)
}

// fakeSeries builds one or more named series over the selected window, per the panel's legend.
func fakeSeries(c *domain.Cluster, p PanelSpec, seed string, window time.Duration) []Series {
	names := fakeSeriesNames(c, p)
	out := make([]Series, 0, len(names))
	for _, n := range names {
		base, amp := fakeSeriesShape(p, n)
		out = append(out, Series{Name: n, Points: fakePoints(seed+"|"+n, base, amp, window)})
	}
	return out
}

// fakeSeriesNames picks the series names for a panel: node names for instance-legend panels, or a
// small fixed label set otherwise (a single unnamed series when the panel has no legend).
func fakeSeriesNames(c *domain.Cluster, p PanelSpec) []string {
	switch p.Legend {
	case "node":
		// etcd/scheduler-family panels only run on control planes; everything else (kubelet,
		// node-exporter) runs on every node - mirrors the real byNode()-joined queries.
		switch p.ID {
		case "etcd_db_size", "etcd_fsync_p99", "etcd_commit_p99":
			return nodeNames(c, domain.RoleControlPlane)
		default:
			return nodeNames(c, "")
		}
	case "code":
		return []string{"200", "201", "403", "404", "500"}
	case "verb":
		return []string{"read", "write"}
	case "namespace":
		names := make([]string, 0, len(fakeNamespaces))
		for _, ns := range fakeNamespaces {
			names = append(names, ns.name)
		}
		return names
	case "phase":
		return []string{"Running", "Pending", "Succeeded", "Failed"}
	case "request_kind":
		return []string{"readOnly", "mutating"}
	case "result":
		return []string{"scheduled", "unschedulable", "error"}
	case "name":
		return []string{"deployment", "replicaset", "endpoint", "node"}
	case "proto":
		return []string{"udp", "tcp"}
	case "type":
		return []string{"A", "AAAA", "PTR"}
	case "direction":
		return []string{"receive", "transmit"}
	case "reason":
		return []string{"Policy denied", "Invalid packet", "CT: Map insertion failed"}
	default:
		return []string{p.Title}
	}
}

// ratioBase gives small-ratio panels (error ratios, saturation) a realistic tiny baseline instead of
// the ~0.4 utilisation default.
var ratioBase = map[string]float64{
	"apiserver_errors": 0.003, "tcp_retrans": 0.012, "disk_sat_by_node": 0.06, "fs_by_node": 0.34, "load_by_node": 0.3,
}

// latencyBase gives each p99-latency panel (unit "s") its own realistic magnitude.
var latencyBase = map[string]float64{
	"apiserver_latency": 0.05, "etcd_fsync_p99": 0.0035, "etcd_commit_p99": 0.018,
	"scheduler_latency_p99": 0.012, "controller_wq_latency": 0.0015, "kubeproxy_sync_p99": 0.02,
	"coredns_latency_p99": 0.003, "kubelet_pleg_p99": 0.05,
}

// opsBase gives each ops/s panel its own realistic magnitude (major page faults are rare; disk
// IOPS on a busy VM are not).
var opsBase = map[string]float64{
	"memsat_by_node": 0.4, "disk_iops_by_node": 55,
}

// nsCPU is fakeNamespaces' per-namespace CPU baseline, for the by-namespace cores series.
func nsCPU(name string) float64 {
	for _, ns := range fakeNamespaces {
		if ns.name == name {
			return ns.cpu
		}
	}
	return 0.05
}

// phaseCount is the pods-by-phase baseline: mostly Running, a few completed, the odd straggler.
func phaseCount(name string) (float64, float64) {
	switch name {
	case "Running":
		return 26, 3
	case "Succeeded":
		return 3, 1
	case "Pending":
		return 0.4, 0.6
	default: // Failed
		return 0.1, 0.2
	}
}

// fakeSeriesShape returns a (baseline, amplitude) for one series, in the panel's unit's natural range.
func fakeSeriesShape(p PanelSpec, name string) (float64, float64) {
	switch p.Unit {
	case "ratio":
		if b, ok := ratioBase[p.ID]; ok {
			return b, b * 0.5
		}
		return 0.35, 0.15 // per-node CPU/memory utilisation
	case "s": // latency in seconds
		b := latencyBase[p.ID]
		if b == 0 {
			b = 0.02
		}
		if name == "write" { // writes run a touch slower than reads
			b *= 1.6
		}
		return b, b * 0.4
	case "bytes": // etcd db size ~ 20–40 MiB
		return 26 * 1024 * 1024, 6 * 1024 * 1024
	case "Bps":
		if p.ID == "disk_thru_by_node" {
			return 5.5e6, 3e6
		}
		return 2.6e6, 1.2e6
	case "cores": // per-namespace CPU usage
		b := nsCPU(name)
		return b, b * 0.4
	case "pps":
		if p.ID == "cilium_drops" && name != "Policy denied" {
			return 0.2, 0.4 // occasional non-policy drops
		}
		if p.ID == "cilium_drops" {
			return 0.05, 0.1
		}
		if p.ID == "net_drops" {
			return 0.3, 0.5 // NIC drops are rare on a healthy node
		}
		return 1800, 700
	case "ops":
		if b, ok := opsBase[p.ID]; ok {
			return b, b * 0.6
		}
		return 12, 6
	case "count":
		if p.Legend == "phase" {
			return phaseCount(name)
		}
		if name == "mutating" { // in-flight requests: reads dominate
			return 2, 1.5
		}
		return 7, 4
	case "rps":
		switch name {
		case "unschedulable", "error", "404", "403", "500":
			return 0.05, 0.15 // rare
		default:
			return 6, 4
		}
	default:
		return 5, 3
	}
}

// fakePoints emits samples across the selected window at StepFor(window) spacing (matching the real
// querier's query_range density) with a per-series sine drift, clamped to ≥0.
func fakePoints(seed string, base, amp float64, window time.Duration) []Point {
	step := StepFor(window)
	now := time.Now()
	start := now.Add(-window)
	ph := phase(seed)
	pts := make([]Point, 0, int(window/step)+1)
	for t := start; !t.After(now); t = t.Add(step) {
		x := float64(t.Unix())
		v := base + amp*math.Sin(x/420+ph) + 0.15*amp*math.Sin(x/97+ph*2)
		if v < 0 {
			v = 0
		}
		pts = append(pts, Point{T: float64(t.Unix()), V: v})
	}
	return pts
}

// fakeStatus synthesizes the status grid, mirroring how the real querier groups multiple raw
// Prometheus series into one row per component (toStatus in monitoring/promql): a DaemonSet-style
// component (kube-proxy, Kubelet, CoreDNS - one instance per node) shows "N/N up", a singleton
// control-plane component (API server, etcd, controller-manager, scheduler) shows a plain status.
func fakeStatus(c *domain.Cluster, id string) []StatusRow {
	if id == "cilium_up" {
		return []StatusRow{
			{Label: "cilium-agent", Up: true, Detail: fmt.Sprintf("%d/%d up", max(len(c.Nodes), 1), max(len(c.Nodes), 1))},
			{Label: "cilium-operator", Up: true, Detail: "operator healthy"},
		}
	}
	n := max(len(c.Nodes), 1)
	perNode := func() string { return fmt.Sprintf("%d/%d up", n, n) }
	return []StatusRow{
		{Label: "API server", Up: true, Detail: "responding"},
		{Label: "etcd", Up: true, Detail: "leader elected"},
		{Label: "Controller manager", Up: true, Detail: "leading"},
		{Label: "Scheduler", Up: true, Detail: "leading"},
		{Label: "kube-proxy", Up: true, Detail: perNode()},
		{Label: "CoreDNS", Up: true, Detail: "serving DNS"},
		{Label: "Kubelet", Up: true, Detail: perNode()},
	}
}

// fakeAlerts always includes the Watchdog (kube-prometheus-stack fires it by design to prove the
// pipeline works), plus an occasional benign warning so the tab isn't static.
func fakeAlerts(seed string) []Alert {
	alerts := []Alert{{
		Name:     "Watchdog",
		Severity: "none",
		State:    "firing",
		Summary:  "An always-firing alert to ensure the entire alerting pipeline is functional.",
		ActiveAt: time.Now().Add(-3 * time.Hour),
	}}
	if osc(seed, 1.0/1800) > 0.6 {
		alerts = append(alerts, Alert{
			Name:     "CPUThrottlingHigh",
			Severity: "info",
			State:    "pending",
			Summary:  "Processes are being throttled on some pods; consider raising CPU limits.",
			ActiveAt: time.Now().Add(-8 * time.Minute),
		})
	}
	return alerts
}

// nodeNames returns the cluster's node names (optionally filtered to a role), falling back to a
// synthetic single node so instance-legend panels are never empty.
func nodeNames(c *domain.Cluster, role domain.Role) []string {
	var out []string
	for _, n := range c.Nodes {
		if role != "" && n.Role != role {
			continue
		}
		name := n.VMName
		if name == "" {
			name = n.ID
		}
		if name != "" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		out = []string{c.Name + "-node"}
	}
	return out
}

// osc returns a value in [-1, 1] - a sine of time (freq in Hz-ish) phased by seed.
func osc(seed string, freq float64) float64 {
	return math.Sin(float64(time.Now().Unix())*freq + phase(seed))
}

func phase(key string) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return float64(h.Sum32()%360) * math.Pi / 180
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
func ptr(f float64) *float64          { return &f }
