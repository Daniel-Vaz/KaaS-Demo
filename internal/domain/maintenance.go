package domain

import (
	"fmt"
	"strings"
	"time"
)

// MaintenanceWindow is the recurring weekly period during which the platform is allowed to perform
// DISRUPTIVE, non-urgent maintenance on a running cluster. It exists because long-term cluster
// management eventually needs an operation that costs a brief outage - etcd defragmentation is the
// first (see EtcdDefragPolicy) - and "self-healing" is not a licence to take one at 14:00 on a
// Tuesday.
//
// It is deployment-level (KAAS_MAINTENANCE_WINDOW / KAAS_MAINTENANCE_TZ), not per cluster: a
// per-cluster override is an additive change later - the window is already a value on the policy,
// so a cluster-level one only has to be read from the row instead of the config.
//
// The zero value ALLOWS EVERYTHING. That is the deliberate default: an operator who has not
// expressed a preference gets a platform that keeps their clusters healthy, not one that silently
// never maintains them. Urgent work bypasses the window entirely (an armed NOSPACE alarm means the
// cluster is ALREADY read-only - waiting for Sunday is strictly worse than a defrag now).
type MaintenanceWindow struct {
	// Days the window is open on. Empty means every day.
	Days map[time.Weekday]bool
	// Start and End are minutes since midnight in Loc. End <= Start means the window wraps past
	// midnight (22:00-02:00), and the day check then applies to the day the window OPENS.
	Start, End int
	// Loc is the timezone the window is expressed in. nil means UTC.
	Loc *time.Location
	// set distinguishes a configured window from the zero value; without it a window parsed as
	// "00:00-00:00" would be indistinguishable from "unconfigured, allow everything".
	set bool
}

// Configured reports whether a window was actually set. An unconfigured window allows any time.
func (w MaintenanceWindow) Configured() bool { return w.set }

// Allows reports whether t falls inside the window. An unconfigured window allows everything.
func (w MaintenanceWindow) Allows(t time.Time) bool {
	if !w.set {
		return true
	}
	loc := w.Loc
	if loc == nil {
		loc = time.UTC
	}
	lt := t.In(loc)
	mins := lt.Hour()*60 + lt.Minute()
	if w.End > w.Start { // ordinary same-day window
		return w.dayOK(lt.Weekday()) && mins >= w.Start && mins < w.End
	}
	// Wrapping window: either late on an open day, or early on the day AFTER an open day.
	if mins >= w.Start {
		return w.dayOK(lt.Weekday())
	}
	if mins < w.End {
		return w.dayOK((lt.Weekday() + 6) % 7) // yesterday
	}
	return false
}

func (w MaintenanceWindow) dayOK(d time.Weekday) bool { return len(w.Days) == 0 || w.Days[d] }

// String renders the window the way it is configured, for events and logs.
func (w MaintenanceWindow) String() string {
	if !w.set {
		return "always"
	}
	zone := "UTC"
	if w.Loc != nil {
		zone = w.Loc.String()
	}
	days := "daily"
	if len(w.Days) > 0 {
		var names []string
		for d := time.Sunday; d <= time.Saturday; d++ {
			if w.Days[d] {
				names = append(names, d.String()[:3])
			}
		}
		days = strings.Join(names, ",")
	}
	return fmt.Sprintf("%s %02d:%02d-%02d:%02d %s", days, w.Start/60, w.Start%60, w.End/60, w.End%60, zone)
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

// ParseMaintenanceWindow parses "[days ]HH:MM-HH:MM", where days is an optional comma-separated
// list of weekday prefixes - "Sun 02:00-06:00", "Sat,Sun 01:00-05:00", "02:00-04:00" (daily),
// "22:00-02:00" (wraps past midnight). An empty spec yields the zero window, which allows any time.
// tz names an IANA location ("Europe/Lisbon"); empty means UTC.
func ParseMaintenanceWindow(spec, tz string) (MaintenanceWindow, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return MaintenanceWindow{}, nil
	}
	loc := time.UTC
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return MaintenanceWindow{}, fmt.Errorf("maintenance window: timezone %q: %w", tz, err)
		}
		loc = l
	}
	w := MaintenanceWindow{Loc: loc, set: true}

	fields := strings.Fields(spec)
	var span string
	switch len(fields) {
	case 1:
		span = fields[0]
	case 2:
		w.Days = map[time.Weekday]bool{}
		for _, name := range strings.Split(fields[0], ",") {
			d, ok := weekdayNames[strings.ToLower(strings.TrimSpace(name))]
			if !ok {
				return MaintenanceWindow{}, fmt.Errorf("maintenance window: unknown weekday %q", name)
			}
			w.Days[d] = true
		}
		span = fields[1]
	default:
		return MaintenanceWindow{}, fmt.Errorf("maintenance window: want %q or %q, got %q", "HH:MM-HH:MM", "Days HH:MM-HH:MM", spec)
	}

	from, to, ok := strings.Cut(span, "-")
	if !ok {
		return MaintenanceWindow{}, fmt.Errorf("maintenance window: %q is not HH:MM-HH:MM", span)
	}
	var err error
	if w.Start, err = parseClock(from); err != nil {
		return MaintenanceWindow{}, err
	}
	if w.End, err = parseClock(to); err != nil {
		return MaintenanceWindow{}, err
	}
	if w.Start == w.End {
		return MaintenanceWindow{}, fmt.Errorf("maintenance window: %q is empty", span)
	}
	return w, nil
}

// parseClock turns "HH:MM" into minutes since midnight.
func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("maintenance window: %q is not HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}
