package domain

import "testing"

func TestRollupHealth(t *testing.T) {
	c := func(s HealthStatus) HealthCheck { return HealthCheck{Status: s} }

	cases := []struct {
		name   string
		checks []HealthCheck
		want   HealthStatus
	}{
		{"empty", nil, HealthUnknown},
		{"all unknown", []HealthCheck{c(HealthUnknown), c(HealthUnknown)}, HealthUnknown},
		{"all healthy", []HealthCheck{c(HealthHealthy), c(HealthHealthy)}, HealthHealthy},
		{"unknown ignored", []HealthCheck{c(HealthHealthy), c(HealthUnknown)}, HealthHealthy},
		{"degraded wins over healthy", []HealthCheck{c(HealthHealthy), c(HealthDegraded)}, HealthDegraded},
		{"unhealthy is worst", []HealthCheck{c(HealthDegraded), c(HealthUnhealthy), c(HealthHealthy)}, HealthUnhealthy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RollupHealth(tc.checks); got != tc.want {
				t.Errorf("RollupHealth = %s, want %s", got, tc.want)
			}
		})
	}
}
