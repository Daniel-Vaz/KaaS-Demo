// Package promql is the real monitoring.Querier: it answers the Monitoring page's panels by querying
// the cluster's in-cluster Prometheus (installed by kube-prometheus-stack) over the Kubernetes API
// server's service proxy - `kubectl get --raw /api/v1/namespaces/<ns>/services/<prom>:9090/proxy/...`.
//
// It runs those kubectl invocations through the same Execer the Workloads seam uses (a LocalExecer on
// the worker, or the API-side proxy that forwards to the worker exec agent - see internal/kube), so
// no new API↔worker transport is added: a monitoring query is just another `kubectl get --raw`.
package promql

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube/kubectl"
	"github.com/Daniel-Vaz/KaaS-demo/internal/monitoring"
	"github.com/Daniel-Vaz/KaaS-demo/internal/tunnel"
)

// Execer runs a one-shot kubectl command for a cluster (structurally satisfied by the Workloads
// seam's LocalExecer and proxy Execer). Only Run is needed - monitoring never streams.
type Execer interface {
	Run(ctx context.Context, kubeconfig []byte, clusterID string, args []string) (kubectl.Result, error)
}

// maxConcurrent caps how many panel queries run at once against one cluster, so a tab refresh doesn't
// fan out an unbounded burst of kubectl invocations at the worker exec agent.
const maxConcurrent = 6

// Querier implements monitoring.Querier on top of an Execer.
type Querier struct{ ex Execer }

// New returns a promql-backed monitoring.Querier using ex to run kubectl.
func New(ex Execer) *Querier { return &Querier{ex: ex} }

// proxyBase is the API-server service-proxy prefix for the cluster's Prometheus, INCLUDING
// Prometheus's own route-prefix. kube-prometheus-stack is installed with a per-cluster
// `--web.route-prefix` (= tunnel.RoutePrefix) so the Monitoring page's "Open UI" link can serve
// Prometheus's web UI under a subpath without breaking its absolute-path assets - but that relocates
// EVERY Prometheus route, its query API included, so every query here must carry the same prefix.
func proxyBase(clusterID string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%s/proxy%s",
		monitoring.Namespace, monitoring.PrometheusService, monitoring.PrometheusPort,
		tunnel.RoutePrefix(clusterID, "prometheus"))
}

func (q *Querier) Tab(ctx context.Context, c *domain.Cluster, kc []byte, tabID string, window time.Duration) (*monitoring.TabData, error) {
	specs, ok := tabPanels(tabID)
	if !ok {
		return nil, monitoring.ErrUnknownTab
	}
	out := &monitoring.TabData{Tab: tabID, GeneratedAt: time.Now(), Panels: make([]monitoring.PanelResult, len(specs))}
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec monitoring.PanelSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out.Panels[i] = q.panel(ctx, c, kc, spec, window)
		}(i, spec)
	}
	wg.Wait()
	return out, nil
}

// tabPanels reads the shared registry so the real querier and the fake resolve the same panel set.
func tabPanels(id string) ([]monitoring.PanelSpec, bool) {
	for _, t := range monitoring.Tabs {
		if t.ID == id {
			return t.Panels, true
		}
	}
	return nil, false
}

func (q *Querier) panel(ctx context.Context, c *domain.Cluster, kc []byte, p monitoring.PanelSpec, window time.Duration) monitoring.PanelResult {
	r := monitoring.PanelResult{ID: p.ID, Title: p.Title, Unit: p.Unit, Kind: p.Kind,
		Section: p.Section, Desc: p.Desc, Viz: p.Viz, Featured: p.Featured}
	if p.Kind == monitoring.KindAlerts {
		alerts, err := q.alerts(ctx, c, kc)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.Alerts = alerts
		return r
	}
	series, err := q.query(ctx, c, kc, p, window)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	switch p.Kind {
	case monitoring.KindSLO, monitoring.KindGauge:
		v, ok := scalarOf(series)
		if !ok {
			r.Error = "no data"
			return r
		}
		r.Value = &v
		if p.Kind == monitoring.KindSLO {
			t := p.Target
			r.Target = &t
		}
	case monitoring.KindStat:
		if p.Range { // sparkline stat: the window's trend plus its latest sample as the headline value
			r.Series = toSeries(series, p)
			v, ok := lastOf(r.Series)
			if !ok {
				r.Error = "no data"
				return r
			}
			r.Value = &v
			return r
		}
		v, ok := scalarOf(series)
		if !ok {
			r.Error = "no data"
			return r
		}
		r.Value = &v
	case monitoring.KindTimeSeries:
		r.Series = toSeries(series, p)
	case monitoring.KindBars:
		r.Bars = toBars(series, p)
	case monitoring.KindStatus:
		r.Rows = toStatus(series, p)
	}
	return r
}

// query runs a panel's instant or range query and returns the parsed Prometheus result set. For a
// range panel the window sets the lookback and StepFor(window) the resolution; instant panels ignore
// it (point-in-time).
func (q *Querier) query(ctx context.Context, c *domain.Cluster, kc []byte, p monitoring.PanelSpec, window time.Duration) ([]promSeries, error) {
	var path string
	if p.Range {
		end := time.Now()
		start := end.Add(-window)
		step := monitoring.StepFor(window)
		v := url.Values{}
		v.Set("query", p.Query)
		v.Set("start", strconv.FormatInt(start.Unix(), 10))
		v.Set("end", strconv.FormatInt(end.Unix(), 10))
		v.Set("step", strconv.FormatInt(int64(step.Seconds()), 10))
		path = proxyBase(c.ID) + "/api/v1/query_range?" + v.Encode()
	} else {
		path = proxyBase(c.ID) + "/api/v1/query?query=" + url.QueryEscape(p.Query)
	}
	return q.rawQuery(ctx, c, kc, path)
}

func (q *Querier) rawQuery(ctx context.Context, c *domain.Cluster, kc []byte, path string) ([]promSeries, error) {
	res, err := q.ex.Run(ctx, kc, c.ID, []string{"get", "--raw", path})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("prometheus query failed: %s", firstLine(res.Stderr))
	}
	var pr promResp
	if err := json.Unmarshal(res.Stdout, &pr); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if pr.Status != "success" {
		if pr.Error != "" {
			return nil, fmt.Errorf("prometheus: %s", pr.Error)
		}
		return nil, fmt.Errorf("prometheus: status %q", pr.Status)
	}
	return pr.Data.Result, nil
}

// alerts fetches active alerts from Prometheus (the stack's shipped alerting rules).
func (q *Querier) alerts(ctx context.Context, c *domain.Cluster, kc []byte) ([]monitoring.Alert, error) {
	res, err := q.ex.Run(ctx, kc, c.ID, []string{"get", "--raw", proxyBase(c.ID) + "/api/v1/alerts"})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("alerts query failed: %s", firstLine(res.Stderr))
	}
	var ar alertsResp
	if err := json.Unmarshal(res.Stdout, &ar); err != nil {
		return nil, fmt.Errorf("decode alerts response: %w", err)
	}
	out := make([]monitoring.Alert, 0, len(ar.Data.Alerts))
	for _, a := range ar.Data.Alerts {
		summary := a.Annotations["summary"]
		if summary == "" {
			summary = a.Annotations["description"]
		}
		sev := a.Labels["severity"]
		if sev == "" {
			sev = "none"
		}
		out = append(out, monitoring.Alert{
			Name:     a.Labels["alertname"],
			Severity: sev,
			State:    a.State,
			Summary:  summary,
			ActiveAt: a.ActiveAt,
		})
	}
	return out, nil
}

// lastOf returns the newest sample of a sparkline stat's series - the first non-empty series' last
// point (a sparkline stat's query is a cluster-wide sum, so there is one series and its points come
// back time-ordered).
func lastOf(series []monitoring.Series) (float64, bool) {
	for _, s := range series {
		if len(s.Points) > 0 {
			return s.Points[len(s.Points)-1].V, true
		}
	}
	return 0, false
}

// toBars maps an instant top-k vector to a bar list sorted largest-first, named by the panel's
// legend label(s). NaN samples (e.g. a `> 0`-filtered empty result) are skipped - an empty list is a
// valid result the portal renders as "nothing to report", not an error.
func toBars(result []promSeries, p monitoring.PanelSpec) []monitoring.Bar {
	out := make([]monitoring.Bar, 0, len(result))
	for _, s := range result {
		if math.IsNaN(s.Value.V) {
			continue
		}
		name := monitoring.LegendName(p.Legend, s.Metric)
		if name == "" {
			name = p.Title
		}
		out = append(out, monitoring.Bar{Name: name, Value: s.Value.V})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out
}

// scalarOf returns the single value of an instant query (the first series' sample).
func scalarOf(series []promSeries) (float64, bool) {
	for _, s := range series {
		if math.IsNaN(s.Value.V) {
			continue
		}
		return s.Value.V, true
	}
	return 0, false
}

// toSeries maps a range result to named series, legending on the panel's label(s) (falling back to
// the panel title for a single unnamed series).
func toSeries(result []promSeries, p monitoring.PanelSpec) []monitoring.Series {
	out := make([]monitoring.Series, 0, len(result))
	for _, s := range result {
		name := monitoring.LegendName(p.Legend, s.Metric)
		if name == "" {
			name = p.Title
		}
		pts := make([]monitoring.Point, 0, len(s.Values))
		for _, v := range s.Values {
			if math.IsNaN(v.V) {
				continue
			}
			pts = append(pts, monitoring.Point{T: v.T, V: v.V})
		}
		out = append(out, monitoring.Series{Name: name, Points: pts})
	}
	return out
}

// toStatus maps an `up{...}` vector to rows: one per component, up iff the sample is 1.
// toStatus maps an `up{...}` vector to one row per component, grouping raw series that share a
// friendly label instead of emitting one row per series. This matters because several components
// scrape as one series PER NODE - kube-proxy and Kubelet are DaemonSet-style (one instance per node),
// and CoreDNS/etcd can run multiple replicas - so without grouping, e.g. a 3-node cluster's kube-proxy
// would show as three separate "kube-proxy" rows instead of one. A component is Up only if every one
// of its instances is; the detail shows "N/M up" for a multi-instance component, or a plain
// "up"/"down" for a singleton (matching how internal/monitoring's Fake presents the same grid, so
// fake and real modes read the same way).
func toStatus(result []promSeries, p monitoring.PanelSpec) []monitoring.StatusRow {
	type agg struct{ up, total int }
	byLabel := make(map[string]*agg, len(result))
	for _, s := range result {
		label := s.Metric[p.Legend]
		if fr, ok := friendlyJob[label]; ok {
			label = fr
		}
		if label == "" {
			label = "component"
		}
		a, ok := byLabel[label]
		if !ok {
			a = &agg{}
			byLabel[label] = a
		}
		a.total++
		if s.Value.V == 1 {
			a.up++
		}
	}
	labels := make([]string, 0, len(byLabel))
	for label := range byLabel {
		labels = append(labels, label)
	}
	rank := func(label string) int {
		if r, ok := statusOrder[label]; ok {
			return r
		}
		return len(statusOrder) // unknown labels (e.g. Cilium's) sort after every known component
	}
	sort.Slice(labels, func(i, j int) bool {
		ri, rj := rank(labels[i]), rank(labels[j])
		if ri != rj {
			return ri < rj
		}
		return labels[i] < labels[j]
	})
	out := make([]monitoring.StatusRow, 0, len(labels))
	for _, label := range labels {
		a := byLabel[label]
		detail := fmt.Sprintf("%d/%d up", a.up, a.total)
		if a.total == 1 {
			if a.up == 1 {
				detail = "up"
			} else {
				detail = "down"
			}
		}
		out = append(out, monitoring.StatusRow{Label: label, Up: a.up == a.total, Detail: detail})
	}
	return out
}

// statusOrder ranks the well-known control-plane components into a fixed, readable order (unknown
// labels - e.g. Cilium's - sort alphabetically after them, since they're not listed here).
var statusOrder = func() map[string]int {
	order := []string{"API server", "etcd", "Controller manager", "Scheduler", "kube-proxy", "CoreDNS", "Kubelet"}
	m := make(map[string]int, len(order))
	for i, name := range order {
		m[name] = i
	}
	return m
}()

// friendlyJob maps Prometheus `job` labels to display names for the control-plane status grid.
var friendlyJob = map[string]string{
	"apiserver":               "API server",
	"kube-apiserver":          "API server",
	"kube-etcd":               "etcd",
	"etcd":                    "etcd",
	"kube-controller-manager": "Controller manager",
	"kube-scheduler":          "Scheduler",
	"kube-proxy":              "kube-proxy",
	"coredns":                 "CoreDNS",
	"kube-dns":                "CoreDNS",
	"kubelet":                 "Kubelet",
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	if s == "" {
		return "no output"
	}
	return s
}

// --- Prometheus HTTP API wire types -----------------------------------------

type promResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string       `json:"resultType"`
		Result     []promSeries `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  promSample        `json:"value"`  // instant (vector/scalar)
	Values []promSample      `json:"values"` // range (matrix)
}

// promSample is a Prometheus [<unix-ts number>, "<value string>"] pair.
type promSample struct {
	T float64
	V float64
}

func (s *promSample) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("prometheus sample: want [ts, value], got %d elements", len(raw))
	}
	if err := json.Unmarshal(raw[0], &s.T); err != nil {
		return err
	}
	var vs string
	if err := json.Unmarshal(raw[1], &vs); err != nil {
		return err
	}
	v, err := strconv.ParseFloat(vs, 64)
	if err != nil {
		s.V = math.NaN() // "NaN"/"+Inf" → treated as no-data by callers
		return nil
	}
	s.V = v
	return nil
}

type alertsResp struct {
	Data struct {
		Alerts []struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
			State       string            `json:"state"`
			ActiveAt    time.Time         `json:"activeAt"`
		} `json:"alerts"`
	} `json:"data"`
}
