package security

import (
	"context"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func testCluster() *domain.Cluster {
	return &domain.Cluster{ID: "cl-abc123", Name: "demo", Nodes: []domain.Node{{ID: "n1"}, {ID: "n2"}}}
}

// TestFakeReportsSummaryMatchesFindings guards the core invariant: a report's list-row summary is
// exactly its detail's finding breakdown, so the two views can never disagree.
func TestFakeReportsSummaryMatchesFindings(t *testing.T) {
	f := NewFake()
	c := testCluster()
	for _, kind := range AllKinds {
		reports, err := f.Reports(context.Background(), c, nil, kind)
		if err != nil {
			t.Fatalf("Reports(%s): %v", kind, err)
		}
		if len(reports) == 0 {
			t.Fatalf("Reports(%s): want at least one report", kind)
		}
		for _, r := range reports {
			d, err := f.Report(context.Background(), c, nil, kind, r.Namespace, r.Name)
			if err != nil {
				t.Fatalf("Report(%s, %s/%s): %v", kind, r.Namespace, r.Name, err)
			}
			var got Counts
			for _, fn := range d.Findings {
				got.Add(fn.Severity)
			}
			if got != r.Summary {
				t.Errorf("%s %s/%s: summary %+v != findings breakdown %+v", kind, r.Namespace, r.Name, r.Summary, got)
			}
			if len(d.Findings) != r.Summary.Total() {
				t.Errorf("%s %s/%s: %d findings, summary total %d", kind, r.Namespace, r.Name, len(d.Findings), r.Summary.Total())
			}
		}
	}
}

// TestFakeReportsDeterministic checks the fake is stable across calls (within a time bucket) so
// polling doesn't reshuffle the table under the user.
func TestFakeReportsDeterministic(t *testing.T) {
	f := NewFake()
	c := testCluster()
	a, _ := f.Reports(context.Background(), c, nil, KindVulnerability)
	b, _ := f.Reports(context.Background(), c, nil, KindVulnerability)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic report count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Summary != b[i].Summary {
			t.Fatalf("report %d differs between calls: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestFakeOverview(t *testing.T) {
	f := NewFake()
	ov, err := f.Overview(context.Background(), testCluster(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Kinds) != len(AllKinds) {
		t.Fatalf("overview kinds = %d, want %d", len(ov.Kinds), len(AllKinds))
	}
	if len(ov.TopImages) == 0 {
		t.Error("overview: want at least one vulnerable image")
	}
	// Top images must be ordered worst-first.
	for i := 1; i < len(ov.TopImages); i++ {
		if riskScore(ov.TopImages[i-1].Summary) < riskScore(ov.TopImages[i].Summary) {
			t.Errorf("top images not sorted worst-first at %d", i)
		}
	}
	if len(ov.Namespaces) == 0 {
		t.Error("overview: want per-namespace risk rows")
	}
}

func TestParseKind(t *testing.T) {
	if _, ok := ParseKind("vulnerability"); !ok {
		t.Error("vulnerability should parse")
	}
	if _, ok := ParseKind("bogus"); ok {
		t.Error("bogus should not parse")
	}
}

func TestEnabled(t *testing.T) {
	c := &domain.Cluster{Addons: []domain.Addon{{Name: AddonName, Phase: "installed"}}}
	if !Enabled(c) {
		t.Error("cluster with installed trivy-operator should be enabled")
	}
	c2 := &domain.Cluster{Addons: []domain.Addon{{Name: AddonName, Phase: "installing"}}}
	if Enabled(c2) {
		t.Error("cluster with not-yet-installed trivy-operator should not be enabled")
	}
}
