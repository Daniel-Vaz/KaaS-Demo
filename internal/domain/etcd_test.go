package domain

import (
	"strings"
	"testing"
	"time"
)

const mib = 1024 * 1024

func testPolicy() EtcdDefragPolicy {
	return EtcdDefragPolicy{
		Enabled:         true,
		ObserveInterval: 6 * time.Hour,
		MinRatio:        0.45,
		MinBytes:        100 * mib,
		MinInterval:     24 * time.Hour,
	}
}

// status builds an observed store of total size db with ratio fragmentation.
func status(db int64, ratio float64) EtcdStatus {
	return EtcdStatus{
		DBBytes:      db,
		DBInUseBytes: int64(float64(db) * (1 - ratio)),
		QuotaBytes:   8 * 1024 * mib,
		Members:      3,
		ObservedAt:   time.Now(),
	}
}

func TestDefragDueWhenFragmented(t *testing.T) {
	due, reason := testPolicy().DefragDue(status(500*mib, 0.60), 3, time.Now())
	if !due {
		t.Fatalf("a 60%%-fragmented 500MiB store was not due: %s", reason)
	}
	if !strings.Contains(reason, "fragmented") {
		t.Errorf("reason = %q, want it to name the fragmentation", reason)
	}
}

// TestDefragFloorBeatsRatio is the single most important threshold test: a tiny store is routinely
// enormously fragmented in RATIO terms, and without the absolute floor the platform would take a
// stop-the-world outage on every idle cluster forever to reclaim a few megabytes.
func TestDefragFloorBeatsRatio(t *testing.T) {
	due, reason := testPolicy().DefragDue(status(4*mib, 0.90), 3, time.Now())
	if due {
		t.Fatalf("a 4MiB store was defragmented despite the 100MiB floor: %s", reason)
	}
	if !strings.Contains(reason, "floor") {
		t.Errorf("reason = %q, want it to name the floor", reason)
	}
}

func TestDefragBelowRatioThreshold(t *testing.T) {
	if due, _ := testPolicy().DefragDue(status(500*mib, 0.20), 3, time.Now()); due {
		t.Fatal("a 20 percent fragmented store was defragmented below the 45 percent threshold")
	}
}

// TestDefragHysteresis: a cluster whose keyspace is genuinely large stays fragmented after a defrag,
// and without the minimum interval it would defragment on every single observation.
func TestDefragHysteresis(t *testing.T) {
	s := status(500*mib, 0.60)
	recent := time.Now().Add(-time.Hour)
	s.DefraggedAt = &recent

	due, reason := testPolicy().DefragDue(s, 3, time.Now())
	if due {
		t.Fatalf("defragmented again an hour after the last run: %s", reason)
	}
	if !strings.Contains(reason, "minimum interval") {
		t.Errorf("reason = %q, want it to name the minimum interval", reason)
	}

	old := time.Now().Add(-48 * time.Hour)
	s.DefraggedAt = &old
	if due, reason := testPolicy().DefragDue(s, 3, time.Now()); !due {
		t.Fatalf("still blocked 48h after the last run: %s", reason)
	}
}

// TestDefragRefusedWhenMemberMissing is the safety guard, not an economy one: defragmentation blocks
// the member it runs on, so doing it while another member is already unreachable is how a
// three-member cluster loses quorum mid-maintenance.
func TestDefragRefusedWhenMemberMissing(t *testing.T) {
	s := status(500*mib, 0.90)
	s.Members = 2 // one of three did not answer
	due, reason := testPolicy().DefragDue(s, 3, time.Now())
	if due {
		t.Fatal("defragmented with a member unreachable")
	}
	if !strings.Contains(reason, "refusing") {
		t.Errorf("reason = %q, want an explicit refusal", reason)
	}

	// And the refusal outranks even the NOSPACE emergency: a read-only cluster is bad, a cluster
	// that has lost quorum is worse.
	s.Alarms = []string{EtcdAlarmNoSpace}
	if due, _ := testPolicy().DefragDue(s, 3, time.Now()); due {
		t.Fatal("NOSPACE bypassed the unreachable-member guard")
	}
}

// TestDefragAlarmBypassesWindowAndHysteresis: an armed NOSPACE means the cluster is ALREADY
// read-only, so defragmenting is outage recovery, not housekeeping - it must not wait for Sunday.
func TestDefragAlarmBypassesWindowAndHysteresis(t *testing.T) {
	p := testPolicy()
	w, err := ParseMaintenanceWindow("Sun 02:00-04:00", "")
	if err != nil {
		t.Fatal(err)
	}
	p.Window = w

	s := status(8*1024*mib, 0.10) // barely fragmented - ordinarily nowhere near due
	recent := time.Now().Add(-time.Minute)
	s.DefraggedAt = &recent
	s.Alarms = []string{EtcdAlarmNoSpace}

	wednesday := at(t, "2026-07-22 14:00 UTC")
	due, reason := p.DefragDue(s, 3, wednesday)
	if !due {
		t.Fatalf("NOSPACE did not trigger an immediate defragmentation: %s", reason)
	}
	if !strings.Contains(reason, "read-only") {
		t.Errorf("reason = %q, want it to explain the urgency", reason)
	}
}

func TestDefragWaitsForWindow(t *testing.T) {
	p := testPolicy()
	w, err := ParseMaintenanceWindow("Sun 02:00-04:00", "")
	if err != nil {
		t.Fatal(err)
	}
	p.Window = w

	s := status(500*mib, 0.60)
	if due, reason := p.DefragDue(s, 3, at(t, "2026-07-22 14:00 UTC")); due {
		t.Fatalf("defragmented outside the maintenance window: %s", reason)
	}
	if due, reason := p.DefragDue(s, 3, at(t, "2026-07-26 03:00 UTC")); !due {
		t.Fatalf("did not defragment inside the maintenance window: %s", reason)
	}
}

func TestDefragDisabled(t *testing.T) {
	var p EtcdDefragPolicy // zero value
	if due, _ := p.DefragDue(status(8*1024*mib, 0.90), 3, time.Now()); due {
		t.Fatal("the zero policy defragmented")
	}
	if p.ObservationDue(nil, time.Now()) {
		t.Fatal("the zero policy asked for an observation")
	}
}

func TestObservationDue(t *testing.T) {
	p := testPolicy()
	if !p.ObservationDue(nil, time.Now()) {
		t.Fatal("a never-observed cluster was not due for observation")
	}
	fresh := EtcdStatus{ObservedAt: time.Now().Add(-time.Hour)}
	if p.ObservationDue(&fresh, time.Now()) {
		t.Fatal("a cluster observed an hour ago was re-observed within the 6h interval")
	}
	stale := EtcdStatus{ObservedAt: time.Now().Add(-7 * time.Hour)}
	if !p.ObservationDue(&stale, time.Now()) {
		t.Fatal("a cluster observed 7h ago was not due")
	}
}

// TestEtcdMaintenanceDueOnlyWhenSettled: maintenance never races an in-flight transition.
func TestEtcdMaintenanceDueOnlyWhenSettled(t *testing.T) {
	p := testPolicy()
	now := time.Now()
	ready := &Cluster{Phase: PhaseReady, Generation: 2, ObservedGeneration: 2}
	if !ready.EtcdMaintenanceDue(p, now) {
		t.Fatal("a settled Ready cluster was not due")
	}
	drifted := &Cluster{Phase: PhaseReady, Generation: 3, ObservedGeneration: 2}
	if drifted.EtcdMaintenanceDue(p, now) {
		t.Fatal("a cluster with pending desired-state changes was due")
	}
	upgrading := &Cluster{Phase: PhaseUpgrading, Generation: 2, ObservedGeneration: 2}
	if upgrading.EtcdMaintenanceDue(p, now) {
		t.Fatal("an upgrading cluster was due")
	}
}

func TestEffectiveQuotaAndUsage(t *testing.T) {
	// No configured quota means etcd's own 2GiB default - the shape of a cluster the platform has
	// never tuned, and the one whose headroom most needs reporting.
	untuned := EtcdStatus{DBBytes: 1024 * mib}
	if got := untuned.EffectiveQuotaBytes(); got != EtcdDefaultQuotaBytes {
		t.Fatalf("EffectiveQuotaBytes = %d, want the 2GiB default", got)
	}
	if got := untuned.QuotaUsage(); got != 0.5 {
		t.Fatalf("QuotaUsage = %v, want 0.5", got)
	}
}

func TestFragmentationRatioEdges(t *testing.T) {
	if got := (EtcdStatus{}).FragmentationRatio(); got != 0 {
		t.Fatalf("empty store ratio = %v, want 0", got)
	}
	// dbSizeInUse can momentarily exceed dbSize between etcd's own accounting updates; that is 0%
	// fragmented, not negative.
	odd := EtcdStatus{DBBytes: 100, DBInUseBytes: 120}
	if got := odd.FragmentationRatio(); got != 0 {
		t.Fatalf("in-use > total ratio = %v, want 0", got)
	}
}
