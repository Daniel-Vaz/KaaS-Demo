package audit

import (
	"context"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func testCluster() *domain.Cluster {
	return &domain.Cluster{ID: "cl-abc123", Name: "demo", Phase: domain.PhaseReady}
}

// TestFakeEventsNewestFirstAndCapped checks the page is ordered newest-first and honours the limit,
// and that the stats rollup matches the returned events.
func TestFakeEventsNewestFirstAndCapped(t *testing.T) {
	f := NewFake()
	page, err := f.Events(context.Background(), testCluster(), nil, Query{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 50 {
		t.Fatalf("got %d events, want the requested 50", len(page.Events))
	}
	if !page.Truncated {
		t.Error("want Truncated=true when more events exist than the limit")
	}
	for i := 1; i < len(page.Events); i++ {
		if page.Events[i-1].Timestamp < page.Events[i].Timestamp {
			t.Fatalf("events not newest-first at %d: %q before %q", i, page.Events[i-1].Timestamp, page.Events[i].Timestamp)
		}
	}
	if page.Stats.Total != len(page.Events) {
		t.Errorf("stats total %d != events %d", page.Stats.Total, len(page.Events))
	}
	if page.Stats.Users == 0 || page.Stats.Namespaces == 0 {
		t.Errorf("want non-zero actor/namespace rollups, got users=%d namespaces=%d", page.Stats.Users, page.Stats.Namespaces)
	}
	var verbSum int
	for _, v := range page.Stats.ByVerb {
		verbSum += v.Count
	}
	if verbSum != page.Stats.Total {
		t.Errorf("by-verb counts sum to %d, want total %d", verbSum, page.Stats.Total)
	}
}

// TestFakeEventsDeterministic guards that a poll doesn't reshuffle the feed under the user: within a
// single fakeInterval window, back-to-back calls return the same audit IDs in the same order.
func TestFakeEventsDeterministic(t *testing.T) {
	f := NewFake()
	c := testCluster()
	a, _ := f.Events(context.Background(), c, nil, Query{Limit: 100})
	b, _ := f.Events(context.Background(), c, nil, Query{Limit: 100})
	if len(a.Events) != len(b.Events) {
		t.Fatalf("non-deterministic count: %d vs %d", len(a.Events), len(b.Events))
	}
	for i := range a.Events {
		if a.Events[i].AuditID != b.Events[i].AuditID {
			t.Fatalf("event %d differs between calls: %q vs %q", i, a.Events[i].AuditID, b.Events[i].AuditID)
		}
	}
}

// TestFakeEventsFilters checks the shared Query filters narrow the feed.
func TestFakeEventsFilters(t *testing.T) {
	f := NewFake()
	c := testCluster()

	full, _ := f.Events(context.Background(), c, nil, Query{Limit: MaxLimit})
	if full.Stats.Total == 0 {
		t.Fatal("fake produced no events")
	}

	// A verb filter must only return that verb.
	del, _ := f.Events(context.Background(), c, nil, Query{Limit: MaxLimit, Verb: "delete"})
	if len(del.Events) == 0 {
		t.Fatal("want some delete events")
	}
	for _, e := range del.Events {
		if e.Verb != "delete" {
			t.Errorf("verb filter leaked %q", e.Verb)
		}
	}
	if len(del.Events) >= len(full.Events) {
		t.Error("verb filter did not narrow the feed")
	}

	// A resource filter matches the resource type substring.
	secrets, _ := f.Events(context.Background(), c, nil, Query{Limit: MaxLimit, Resource: "secret"})
	for _, e := range secrets.Events {
		if e.Resource.Type != "secrets" {
			t.Errorf("resource filter leaked %q", e.Resource.Type)
		}
	}
}

// TestStatsIndependentOfLimit guards the fix for the redundant "Events" tile: the actor/namespace
// rollups are properties of the data (a small fixed pool), so they stay stable as the row cap grows
// even though the event Total tracks the cap - which is exactly why Total made a poor stat tile.
func TestStatsIndependentOfLimit(t *testing.T) {
	f := NewFake()
	c := testCluster()
	small, _ := f.Events(context.Background(), c, nil, Query{Limit: 50})
	big, _ := f.Events(context.Background(), c, nil, Query{Limit: MaxLimit})

	if big.Stats.Total <= small.Stats.Total {
		t.Fatalf("total should grow with the limit: %d vs %d", small.Stats.Total, big.Stats.Total)
	}
	// The fake draws namespaces from a small fixed pool, so even as Total tracks the cap the
	// distinct-namespace count stays inside that pool: it must not scale with the 20x-larger cap.
	// (A 50-event sample need not surface every namespace, so this is a bound, not exact equality.)
	const nsPool = 6 // distinct namespaces across fakeResources
	if big.Stats.Namespaces > nsPool {
		t.Errorf("namespaces should be data-bounded to the fixed pool of %d, got %d (cap %d)",
			nsPool, big.Stats.Namespaces, MaxLimit)
	}
	if big.Stats.Namespaces < small.Stats.Namespaces {
		t.Errorf("a larger page cannot surface fewer namespaces: %d (cap 50) vs %d (cap %d)",
			small.Stats.Namespaces, big.Stats.Namespaces, MaxLimit)
	}
	if big.Stats.Namespaces >= big.Stats.Total {
		t.Errorf("distinct namespaces (%d) should be far below the event count (%d)", big.Stats.Namespaces, big.Stats.Total)
	}
}

func TestEnabled(t *testing.T) {
	if !Enabled(&domain.Cluster{Phase: domain.PhaseReady}) {
		t.Error("a Ready cluster should be audit-enabled")
	}
	if Enabled(&domain.Cluster{Phase: domain.PhaseProvisioningInfra}) {
		t.Error("a non-Ready cluster should not be audit-enabled")
	}
}
