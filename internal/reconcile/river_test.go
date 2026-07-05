package reconcile

import (
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"
)

// jobParkedInFuture gates whether the enqueue loop pulls a deduped reconcile job forward when a
// cluster is being deleted: only jobs waiting to run later (backed-off retryable / scheduled) need
// it, never an already-available or running job.
func TestJobParkedInFuture(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name string
		job  *rivertype.JobRow
		want bool
	}{
		{"nil", nil, false},
		{"retryable backed off into the future", &rivertype.JobRow{State: rivertype.JobStateRetryable, ScheduledAt: future}, true},
		{"scheduled ahead", &rivertype.JobRow{State: rivertype.JobStateScheduled, ScheduledAt: future}, true},
		{"retryable already due", &rivertype.JobRow{State: rivertype.JobStateRetryable, ScheduledAt: past}, false},
		{"available", &rivertype.JobRow{State: rivertype.JobStateAvailable, ScheduledAt: future}, false},
		{"running", &rivertype.JobRow{State: rivertype.JobStateRunning, ScheduledAt: future}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobParkedInFuture(tc.job); got != tc.want {
				t.Fatalf("jobParkedInFuture = %v, want %v", got, tc.want)
			}
		})
	}
}
