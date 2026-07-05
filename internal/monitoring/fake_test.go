package monitoring

import (
	"context"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func testCluster() *domain.Cluster {
	return &domain.Cluster{
		ID: "c1", Name: "demo", Size: "small",
		Nodes: []domain.Node{
			{VMName: "demo-cp-0", Role: domain.RoleControlPlane},
			{VMName: "demo-w-0", Role: domain.RoleWorker},
			{VMName: "demo-w-1", Role: domain.RoleWorker},
		},
	}
}

// TestFakeResolvesEveryTab checks the fake produces a fully-populated result for every panel in
// every registered tab - the field that matches each panel's kind is set - so the whole page is
// demoable under make up-fake.
func TestFakeResolvesEveryTab(t *testing.T) {
	f := NewFake()
	for _, spec := range Tabs {
		data, err := f.Tab(context.Background(), testCluster(), nil, spec.ID, ParseRange(DefaultRange))
		if err != nil {
			t.Fatalf("tab %q: %v", spec.ID, err)
		}
		if len(data.Panels) != len(spec.Panels) {
			t.Fatalf("tab %q: %d panels, want %d", spec.ID, len(data.Panels), len(spec.Panels))
		}
		for _, p := range data.Panels {
			if p.Error != "" {
				t.Fatalf("tab %q panel %q: unexpected error %q", spec.ID, p.ID, p.Error)
			}
			switch p.Kind {
			case KindSLO:
				if p.Value == nil || p.Target == nil {
					t.Fatalf("panel %q: slo missing value/target", p.ID)
				}
			case KindGauge, KindStat:
				if p.Value == nil {
					t.Fatalf("panel %q: %s missing value", p.ID, p.Kind)
				}
			case KindTimeSeries:
				if len(p.Series) == 0 || len(p.Series[0].Points) == 0 {
					t.Fatalf("panel %q: timeseries empty", p.ID)
				}
			case KindBars:
				if len(p.Bars) == 0 {
					t.Fatalf("panel %q: bars empty", p.ID)
				}
				for i := 1; i < len(p.Bars); i++ {
					if p.Bars[i].Value > p.Bars[i-1].Value {
						t.Fatalf("panel %q: bars not sorted desc: %+v", p.ID, p.Bars)
					}
				}
			case KindStatus:
				if len(p.Rows) == 0 {
					t.Fatalf("panel %q: status grid empty", p.ID)
				}
			case KindAlerts:
				if len(p.Alerts) == 0 {
					t.Fatalf("panel %q: alerts empty", p.ID)
				}
			}
		}
	}
}

// TestFakeSparklineStat: a Range stat (net_throughput on Overview) carries both a series for its
// sparkline and the newest sample as its value, matching the real querier's shape.
func TestFakeSparklineStat(t *testing.T) {
	data, err := NewFake().Tab(context.Background(), testCluster(), nil, "overview", ParseRange(DefaultRange))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range data.Panels {
		if p.ID != "net_throughput" {
			continue
		}
		if p.Value == nil || len(p.Series) == 0 || len(p.Series[0].Points) == 0 {
			t.Fatalf("sparkline stat missing value/series: %+v", p)
		}
		last := p.Series[0].Points[len(p.Series[0].Points)-1].V
		if *p.Value != last {
			t.Fatalf("sparkline stat value %v != newest sample %v", *p.Value, last)
		}
		return
	}
	t.Fatal("net_throughput panel not found on overview")
}

// TestFakeAlertsIncludeWatchdog: the always-firing Watchdog alert is present (kube-prometheus-stack
// fires it by design to prove the alerting pipeline works).
func TestFakeAlertsIncludeWatchdog(t *testing.T) {
	data, err := NewFake().Tab(context.Background(), testCluster(), nil, "alerts", ParseRange(DefaultRange))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range data.Panels[0].Alerts {
		if a.Name == "Watchdog" && a.State == "firing" {
			found = true
		}
	}
	if !found {
		t.Fatal("Watchdog alert not present/firing in the fake alerts tab")
	}
}

func TestFakeUnknownTab(t *testing.T) {
	if _, err := NewFake().Tab(context.Background(), testCluster(), nil, "nope", ParseRange(DefaultRange)); err != ErrUnknownTab {
		t.Fatalf("unknown tab err = %v, want ErrUnknownTab", err)
	}
}

// TestEnabled reflects the kube-prometheus-stack add-on install state.
func TestEnabled(t *testing.T) {
	c := testCluster()
	if Enabled(c) {
		t.Fatal("Enabled with no add-ons, want false")
	}
	c.Addons = []domain.Addon{{Name: AddonName, Phase: "pending"}}
	if Enabled(c) {
		t.Fatal("Enabled while stack still pending, want false")
	}
	c.Addons = []domain.Addon{{Name: AddonName, Phase: "installed"}}
	if !Enabled(c) {
		t.Fatal("Enabled with stack installed, want true")
	}
}
