package promql

import (
	"context"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube/kubectl"
	"github.com/Daniel-Vaz/KaaS-demo/internal/monitoring"
)

// stubExecer returns canned Prometheus JSON chosen by which endpoint the raw path targets, so the
// parsing (vector → scalar, matrix → series, up{} → status, /alerts → alerts) is unit-tested without
// a cluster.
type stubExecer struct{ lastArgs []string }

func (s *stubExecer) Run(_ context.Context, _ []byte, _ string, args []string) (kubectl.Result, error) {
	s.lastArgs = args
	path := args[len(args)-1]
	var body string
	switch {
	case strings.Contains(path, "/api/v1/alerts"):
		body = `{"status":"success","data":{"alerts":[
		  {"labels":{"alertname":"Watchdog","severity":"none"},"annotations":{"summary":"pipeline ok"},"state":"firing","activeAt":"2026-01-01T00:00:00Z"},
		  {"labels":{"alertname":"KubeCPUOvercommit","severity":"warning"},"annotations":{"description":"overcommitted"},"state":"firing","activeAt":"2026-01-01T00:05:00Z"}
		]}}`
	case strings.Contains(path, "query_range"):
		body = `{"status":"success","data":{"resultType":"matrix","result":[
		  {"metric":{"code":"200"},"values":[[1700000000,"5"],[1700000060,"6"]]},
		  {"metric":{"code":"500"},"values":[[1700000000,"0"],[1700000060,"1"]]}
		]}}`
	case strings.Contains(path, `up%7Bjob`) || strings.Contains(path, "up{job"):
		body = `{"status":"success","data":{"resultType":"vector","result":[
		  {"metric":{"job":"apiserver"},"value":[1700000000,"1"]},
		  {"metric":{"job":"kube-scheduler"},"value":[1700000000,"0"]}
		]}}`
	default: // instant scalar
		body = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"0.997"]}]}}`
	}
	return kubectl.Result{Stdout: []byte(body), Code: 0}, nil
}

func newQ() (*Querier, *stubExecer) {
	ex := &stubExecer{}
	return New(ex), ex
}

func TestScalarPanel(t *testing.T) {
	q, _ := newQ()
	r := q.panel(context.Background(), &domain.Cluster{ID: "c"}, nil,
		monitoring.PanelSpec{ID: "slo", Kind: monitoring.KindSLO, Query: "x", Target: 0.995}, monitoring.ParseRange(monitoring.DefaultRange))
	if r.Error != "" {
		t.Fatalf("unexpected error %q", r.Error)
	}
	if r.Value == nil || *r.Value < 0.996 || *r.Value > 0.998 {
		t.Fatalf("slo value = %v, want ~0.997", r.Value)
	}
	if r.Target == nil || *r.Target != 0.995 {
		t.Fatalf("slo target = %v, want 0.995", r.Target)
	}
}

func TestTimeSeriesPanel(t *testing.T) {
	q, _ := newQ()
	r := q.panel(context.Background(), &domain.Cluster{ID: "c"}, nil,
		monitoring.PanelSpec{ID: "rate", Kind: monitoring.KindTimeSeries, Range: true, Legend: "code", Query: "x"}, monitoring.ParseRange(monitoring.DefaultRange))
	if len(r.Series) != 2 {
		t.Fatalf("series = %d, want 2", len(r.Series))
	}
	names := map[string]int{r.Series[0].Name: len(r.Series[0].Points), r.Series[1].Name: len(r.Series[1].Points)}
	if names["200"] != 2 || names["500"] != 2 {
		t.Fatalf("series by code = %v, want 200:2 500:2", names)
	}
}

func TestStatusPanel(t *testing.T) {
	q, _ := newQ()
	r := q.panel(context.Background(), &domain.Cluster{ID: "c"}, nil,
		monitoring.PanelSpec{ID: "components_up", Kind: monitoring.KindStatus, Legend: "job", Query: "up{job=~\"x\"}"}, monitoring.ParseRange(monitoring.DefaultRange))
	if len(r.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(r.Rows))
	}
	byLabel := map[string]bool{r.Rows[0].Label: r.Rows[0].Up, r.Rows[1].Label: r.Rows[1].Up}
	if !byLabel["API server"] {
		t.Fatalf("API server row not up: %v", byLabel)
	}
	if byLabel["Scheduler"] {
		t.Fatalf("Scheduler should be down (up=0): %v", byLabel)
	}
}

func TestAlertsPanel(t *testing.T) {
	q, _ := newQ()
	r := q.panel(context.Background(), &domain.Cluster{ID: "c"}, nil,
		monitoring.PanelSpec{ID: "active_alerts", Kind: monitoring.KindAlerts}, monitoring.ParseRange(monitoring.DefaultRange))
	if len(r.Alerts) != 2 {
		t.Fatalf("alerts = %d, want 2", len(r.Alerts))
	}
	var watchdog, warn bool
	for _, a := range r.Alerts {
		if a.Name == "Watchdog" && a.Severity == "none" && a.Summary == "pipeline ok" {
			watchdog = true
		}
		if a.Name == "KubeCPUOvercommit" && a.Severity == "warning" && a.Summary == "overcommitted" {
			warn = true
		}
	}
	if !watchdog || !warn {
		t.Fatalf("alerts parse mismatch: watchdog=%v warn=%v", watchdog, warn)
	}
}

// TestRangeQueryPath checks a range panel targets query_range with the query URL-escaped.
func TestRangeQueryPath(t *testing.T) {
	q, ex := newQ()
	q.panel(context.Background(), &domain.Cluster{ID: "c"}, nil,
		monitoring.PanelSpec{ID: "r", Kind: monitoring.KindTimeSeries, Range: true, Query: "sum(rate(x[5m]))"}, monitoring.ParseRange(monitoring.DefaultRange))
	path := ex.lastArgs[len(ex.lastArgs)-1]
	if !strings.Contains(path, "/query_range?") {
		t.Fatalf("range path = %q, want query_range", path)
	}
	if !strings.Contains(path, monitoring.PrometheusService) {
		t.Fatalf("path %q missing prometheus service", path)
	}
}

// TestHintsPropagate checks the layout/presentation hints on a PanelSpec (Featured, Section, Desc,
// Viz) ride through to the PanelResult the portal receives.
func TestHintsPropagate(t *testing.T) {
	q, _ := newQ()
	r := q.panel(context.Background(), &domain.Cluster{ID: "c"}, nil,
		monitoring.PanelSpec{ID: "cpu_util", Kind: monitoring.KindGauge, Query: "x",
			Featured: true, Section: "Cluster at a glance", Desc: "how busy", Viz: monitoring.VizArea},
		monitoring.ParseRange(monitoring.DefaultRange))
	if !r.Featured || r.Section != "Cluster at a glance" || r.Desc != "how busy" || r.Viz != monitoring.VizArea {
		t.Fatalf("hints did not propagate from PanelSpec to PanelResult: %+v", r)
	}
}

// TestSparklineStat: a KindStat panel with Range set resolves via query_range and carries BOTH the
// window's series (the sparkline) and the newest sample as its headline value.
func TestSparklineStat(t *testing.T) {
	q, ex := newQ()
	r := q.panel(context.Background(), &domain.Cluster{ID: "c"}, nil,
		monitoring.PanelSpec{ID: "net_throughput", Kind: monitoring.KindStat, Range: true, Query: "sum(x)"},
		monitoring.ParseRange(monitoring.DefaultRange))
	if r.Error != "" {
		t.Fatalf("unexpected error %q", r.Error)
	}
	if !strings.Contains(ex.lastArgs[len(ex.lastArgs)-1], "/query_range?") {
		t.Fatalf("sparkline stat did not use query_range: %v", ex.lastArgs)
	}
	if len(r.Series) == 0 {
		t.Fatal("sparkline stat missing its series")
	}
	// The stub's first matrix series is code=200 with values [5, 6] - the newest sample is 6.
	if r.Value == nil || *r.Value != 6 {
		t.Fatalf("sparkline stat value = %v, want 6 (newest sample)", r.Value)
	}
}

// TestToBars: an instant top-k vector maps to a bar list sorted largest-first, named by the
// panel's legend - including a comma-separated multi-label legend joined with "/".
func TestToBars(t *testing.T) {
	result := []promSeries{
		{Metric: map[string]string{"namespace": "default", "pod": "app-1"}, Value: promSample{V: 2}},
		{Metric: map[string]string{"namespace": "kube-system", "pod": "coredns-x"}, Value: promSample{V: 5}},
	}
	bars := toBars(result, monitoring.PanelSpec{Legend: "namespace,pod"})
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want 2", len(bars))
	}
	if bars[0].Name != "kube-system/coredns-x" || bars[0].Value != 5 {
		t.Fatalf("bars[0] = %+v, want kube-system/coredns-x = 5 (sorted desc)", bars[0])
	}
	if bars[1].Name != "default/app-1" {
		t.Fatalf("bars[1] = %+v, want default/app-1", bars[1])
	}
}

// TestToStatusGroupsMultiInstance: a DaemonSet-style component (kube-proxy: one instance per node)
// must collapse into a SINGLE status row, not one row per node - the bug reported against a live
// cluster where kube-proxy showed as N duplicate entries. Mixed up/down instances report "up: false"
// with an "N/M up" detail; a fully-up component reports the same detail but Up: true.
func TestToStatusGroupsMultiInstance(t *testing.T) {
	result := []promSeries{
		{Metric: map[string]string{"job": "apiserver"}, Value: promSample{V: 1}},
		{Metric: map[string]string{"job": "kube-proxy"}, Value: promSample{V: 1}},
		{Metric: map[string]string{"job": "kube-proxy"}, Value: promSample{V: 1}},
		{Metric: map[string]string{"job": "kube-proxy"}, Value: promSample{V: 0}}, // one node down
	}
	rows := toStatus(result, monitoring.PanelSpec{Legend: "job"})
	byLabel := make(map[string]monitoring.StatusRow, len(rows))
	for _, r := range rows {
		byLabel[r.Label] = r
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one per component, not one per instance): %+v", len(rows), rows)
	}
	kp, ok := byLabel["kube-proxy"]
	if !ok {
		t.Fatalf("no kube-proxy row in %+v", rows)
	}
	if kp.Up {
		t.Fatalf("kube-proxy Up = true, want false (one instance down): %+v", kp)
	}
	if kp.Detail != "2/3 up" {
		t.Fatalf("kube-proxy detail = %q, want \"2/3 up\"", kp.Detail)
	}
	as, ok := byLabel["API server"]
	if !ok || !as.Up || as.Detail != "up" {
		t.Fatalf("API server row = %+v, want {Up:true Detail:\"up\"}", as)
	}
}

// TestToStatusAllUpSingleRow: every kube-proxy instance up still collapses to one row, Up: true.
func TestToStatusAllUpSingleRow(t *testing.T) {
	result := []promSeries{
		{Metric: map[string]string{"job": "kube-proxy"}, Value: promSample{V: 1}},
		{Metric: map[string]string{"job": "kube-proxy"}, Value: promSample{V: 1}},
		{Metric: map[string]string{"job": "kube-proxy"}, Value: promSample{V: 1}},
	}
	rows := toStatus(result, monitoring.PanelSpec{Legend: "job"})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	if !rows[0].Up || rows[0].Detail != "3/3 up" {
		t.Fatalf("row = %+v, want {Up:true Detail:\"3/3 up\"}", rows[0])
	}
}
