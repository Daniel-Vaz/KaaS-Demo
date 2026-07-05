package domain

import (
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02 15:04 MST", s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestMaintenanceWindowUnconfiguredAllowsEverything pins the default that keeps an operator who has
// expressed no preference from silently getting a platform that never maintains their clusters.
func TestMaintenanceWindowUnconfiguredAllowsEverything(t *testing.T) {
	w, err := ParseMaintenanceWindow("", "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Configured() {
		t.Fatal("empty spec produced a configured window")
	}
	if !w.Allows(at(t, "2026-07-22 14:00 UTC")) {
		t.Fatal("unconfigured window refused 14:00 on a Wednesday")
	}
}

func TestMaintenanceWindowDaily(t *testing.T) {
	w, err := ParseMaintenanceWindow("02:00-04:00", "")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		when string
		want bool
	}{
		{"2026-07-22 02:00 UTC", true}, // inclusive at the start
		{"2026-07-22 03:59 UTC", true},
		{"2026-07-22 04:00 UTC", false}, // exclusive at the end
		{"2026-07-22 01:59 UTC", false},
		{"2026-07-25 03:00 UTC", true}, // any weekday
	}
	for _, tc := range cases {
		if got := w.Allows(at(t, tc.when)); got != tc.want {
			t.Errorf("Allows(%s) = %v, want %v", tc.when, got, tc.want)
		}
	}
}

func TestMaintenanceWindowWeekdays(t *testing.T) {
	w, err := ParseMaintenanceWindow("Sat,Sun 01:00-05:00", "")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Allows(at(t, "2026-07-26 03:00 UTC")) { // a Sunday
		t.Error("Sunday 03:00 refused")
	}
	if w.Allows(at(t, "2026-07-22 03:00 UTC")) { // a Wednesday
		t.Error("Wednesday 03:00 allowed")
	}
}

// TestMaintenanceWindowWraps covers the window that crosses midnight - where the day check has to
// apply to the day the window OPENED, not the day the clock now reads.
func TestMaintenanceWindowWraps(t *testing.T) {
	w, err := ParseMaintenanceWindow("Sun 22:00-02:00", "")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		when string
		want bool
	}{
		{"2026-07-26 23:00 UTC", true},  // Sunday night, after it opens
		{"2026-07-27 01:00 UTC", true},  // Monday small hours - still Sunday's window
		{"2026-07-27 03:00 UTC", false}, // Monday, after it closes
		{"2026-07-26 21:00 UTC", false}, // Sunday, before it opens
		{"2026-07-28 01:00 UTC", false}, // Tuesday small hours - Monday's window never opened
	}
	for _, tc := range cases {
		if got := w.Allows(at(t, tc.when)); got != tc.want {
			t.Errorf("Allows(%s) = %v, want %v", tc.when, got, tc.want)
		}
	}
}

// TestMaintenanceWindowTimezone: the window is expressed in the operator's zone, so the same instant
// is inside or outside it depending on the configured location - the whole reason KAAS_MAINTENANCE_TZ
// exists rather than making everyone convert their small hours to UTC.
func TestMaintenanceWindowTimezone(t *testing.T) {
	w, err := ParseMaintenanceWindow("02:00-04:00", "Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	// 02:00 UTC in July is 03:00 in Lisbon (WEST) - inside. 01:00 UTC is 02:00 local - also inside.
	if !w.Allows(at(t, "2026-07-22 02:00 UTC")) {
		t.Error("03:00 Lisbon refused")
	}
	// 04:00 UTC is 05:00 local - outside.
	if w.Allows(at(t, "2026-07-22 04:00 UTC")) {
		t.Error("05:00 Lisbon allowed")
	}
}

func TestParseMaintenanceWindowErrors(t *testing.T) {
	for _, spec := range []string{"02:00", "Funday 02:00-04:00", "2am-4am", "02:00-02:00", "a b c"} {
		if _, err := ParseMaintenanceWindow(spec, ""); err == nil {
			t.Errorf("ParseMaintenanceWindow(%q) accepted a malformed spec", spec)
		}
	}
	if _, err := ParseMaintenanceWindow("02:00-04:00", "Mars/Olympus"); err == nil {
		t.Error("ParseMaintenanceWindow accepted an unknown timezone")
	}
}
