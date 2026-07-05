package domain

import (
	"testing"
	"time"
)

func snapshotTestPolicy() EtcdSnapshotPolicy {
	return EtcdSnapshotPolicy{
		Enabled:       true,
		Interval:      6 * time.Hour,
		Retain:        3,
		MaxRestoreAge: 24 * time.Hour,
	}
}

// TestSnapshotDueBackfillsNeverSnapshotted: a cluster with no backup is always due, which is how the
// fleet backfills itself when the feature is switched on.
func TestSnapshotDueBackfillsNeverSnapshotted(t *testing.T) {
	p := snapshotTestPolicy()
	now := time.Now()
	if !p.SnapshotDue(nil, now) {
		t.Fatal("a never-snapshotted cluster should be due")
	}
	recent := now.Add(-time.Hour)
	if p.SnapshotDue(&recent, now) {
		t.Fatal("a cluster snapshotted an hour ago should not be due under a 6h interval")
	}
	old := now.Add(-7 * time.Hour)
	if !p.SnapshotDue(&old, now) {
		t.Fatal("a cluster snapshotted 7h ago should be due under a 6h interval")
	}
	var off EtcdSnapshotPolicy
	if off.SnapshotDue(nil, now) {
		t.Fatal("a disabled policy reported a snapshot due")
	}
}

// TestEtcdSnapshotDueOnlyWhenReadyAndConverged: a mid-upgrade snapshot captures a cluster half-way
// between two bundles, which is the one state nobody wants to restore to.
func TestEtcdSnapshotDueOnlyWhenReadyAndConverged(t *testing.T) {
	p := snapshotTestPolicy()
	now := time.Now()
	c := &Cluster{Phase: PhaseReady, Generation: 1, ObservedGeneration: 1}
	if !c.EtcdSnapshotDue(p, now) {
		t.Fatal("a Ready, converged, never-snapshotted cluster should be due")
	}
	c.Phase = PhaseUpgrading
	if c.EtcdSnapshotDue(p, now) {
		t.Fatal("a snapshot was due mid-upgrade")
	}
	c.Phase = PhaseReady
	c.Generation = 2
	if c.EtcdSnapshotDue(p, now) {
		t.Fatal("a snapshot was due with desired-state changes pending")
	}
}

// TestRestorableSnapshotPicksNewest: every stored snapshot has already passed verification, so among
// valid candidates the newest is unambiguously the least lossy.
func TestRestorableSnapshotPicksNewest(t *testing.T) {
	p := snapshotTestPolicy()
	now := time.Now()
	snaps := []EtcdSnapshot{
		{ID: "old", TakenAt: now.Add(-10 * time.Hour), Revision: 100},
		{ID: "new", TakenAt: now.Add(-2 * time.Hour), Revision: 300},
		{ID: "mid", TakenAt: now.Add(-5 * time.Hour), Revision: 200},
	}
	got, reason, ok := RestorableSnapshot(snaps, p, now)
	if !ok || got.ID != "new" {
		t.Fatalf("picked %q (%s), want the newest", got.ID, reason)
	}
}

// TestRestorableSnapshotRefusesStale: putting back a keyspace older than the limit is not obviously
// better than the outage it replaces, and is a decision for a human rather than a control loop.
func TestRestorableSnapshotRefusesStale(t *testing.T) {
	p := snapshotTestPolicy()
	now := time.Now()
	snaps := []EtcdSnapshot{{ID: "ancient", TakenAt: now.Add(-72 * time.Hour)}}
	if _, reason, ok := RestorableSnapshot(snaps, p, now); ok {
		t.Fatalf("restored a 3-day-old snapshot under a 24h limit (%s)", reason)
	}
}

// TestRestorableSnapshotWithNone explains itself, because "why is my cluster not coming back" is the
// question being asked at that moment.
func TestRestorableSnapshotWithNone(t *testing.T) {
	_, reason, ok := RestorableSnapshot(nil, snapshotTestPolicy(), time.Now())
	if ok {
		t.Fatal("reported a restorable snapshot from an empty set")
	}
	if reason == "" {
		t.Fatal("expected a reason")
	}
}

// TestPruneKeepsNewest and, crucially, never prunes to nothing.
func TestPruneKeepsNewest(t *testing.T) {
	p := snapshotTestPolicy()
	now := time.Now()
	var snaps []EtcdSnapshot
	for i := range 5 {
		snaps = append(snaps, EtcdSnapshot{ID: string(rune('a' + i)), TakenAt: now.Add(-time.Duration(i) * time.Hour)})
	}
	drop := p.PruneEtcdSnapshots(snaps)
	if len(drop) != 2 {
		t.Fatalf("dropping %d of 5 with Retain=3, want 2", len(drop))
	}
	for _, d := range drop {
		if d.ID == "a" || d.ID == "b" || d.ID == "c" {
			t.Fatalf("pruned %q, which is among the newest three", d.ID)
		}
	}

	if got := p.PruneEtcdSnapshots(snaps[:2]); got != nil {
		t.Fatalf("pruned %d snapshots when fewer than Retain exist", len(got))
	}

	// A misconfigured Retain must not delete the last backup - a retention policy that leaves nothing
	// is not a retention policy.
	zero := EtcdSnapshotPolicy{Enabled: true, Retain: 0}
	if drop := zero.PruneEtcdSnapshots(snaps); len(drop) != 4 {
		t.Fatalf("Retain=0 dropped %d of 5, want 4 (one kept regardless)", len(drop))
	}
}

func TestHumanDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{72 * time.Hour, "3d"},
	} {
		if got := HumanDuration(tc.d); got != tc.want {
			t.Errorf("HumanDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
