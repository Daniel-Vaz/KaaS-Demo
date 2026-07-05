package domain

import (
	"fmt"
	"time"
)

// EtcdDefaultQuotaBytes is etcd's own default backend-store quota (2GiB) - the ceiling a cluster
// runs with when nothing has raised it. Used as the denominator for a member the platform has not
// tuned (one bootstrapped before the controlplane_etcd role existed), so the quota-headroom health
// check means something on every cluster rather than going Unknown on exactly the ones most at risk.
const EtcdDefaultQuotaBytes int64 = 2 * 1024 * 1024 * 1024

// EtcdAlarmNoSpace and EtcdAlarmCorrupt are the two alarms etcd arms on itself. NOSPACE is the one
// this whole feature exists for: it fires when the backend store hits the quota and makes the entire
// cluster READ-ONLY until a human (or us) defragments and disarms it. CORRUPT means the members
// disagree about data and is NOT something defragmentation fixes - it is surfaced, never acted on.
const (
	EtcdAlarmNoSpace = "NOSPACE"
	EtcdAlarmCorrupt = "CORRUPT"
)

// EtcdStatus is OBSERVED state about a cluster's etcd backend store, in the same category as
// CertNotAfter: read on a slow cadence by config.Manager.EtcdStatus, stamped on the cluster row, and
// then used both as the level-triggered signal for automatic defragmentation and as the input to the
// etcd-store health check. Because it is stored observed state rather than a live probe, the fake
// and real backends report the health check identically - the same trick health.CertCheck plays.
//
// The size fields are the WORST member's, not an average: defragmentation is decided per cluster and
// a three-member cluster is as fragmented as its most fragmented member. nil on a cluster whose etcd
// has never been observed.
type EtcdStatus struct {
	// DBBytes is the physical size of the bbolt backend file - what counts against the quota.
	DBBytes int64 `json:"db_bytes"`
	// DBInUseBytes is the logically-used portion of it. The gap between the two is the fragmentation
	// that only defragmentation reclaims: compaction (the apiserver's, every 5m) frees keyspace
	// logically and never shrinks the file.
	DBInUseBytes int64 `json:"db_in_use_bytes"`
	// QuotaBytes is the member's configured --quota-backend-bytes, read off its static-pod manifest.
	// 0 means the flag is absent, i.e. etcd's 2GiB default - see EffectiveQuotaBytes.
	QuotaBytes int64 `json:"quota_bytes,omitempty"`
	// Alarms are the alarms currently armed cluster-wide (EtcdAlarmNoSpace, EtcdAlarmCorrupt).
	Alarms []string `json:"alarms,omitempty"`
	// Members is how many members answered the status read. Below the cluster's control-plane count
	// it means a member is unreachable - which is a hard block on defragmenting (see DefragDue).
	Members int `json:"members"`
	// ObservedAt is when this snapshot was taken; it drives the re-observation cadence.
	ObservedAt time.Time `json:"observed_at"`
	// DefraggedAt is when the platform last defragmented this cluster, or nil if it never has. This
	// is the hysteresis floor: without it a cluster that stays fragmented after a defrag (because
	// its keyspace genuinely is that big) would re-defragment on every single tick.
	DefraggedAt *time.Time `json:"defragged_at,omitempty"`
}

// EffectiveQuotaBytes is the ceiling this cluster's etcd actually enforces: its configured quota, or
// etcd's 2GiB default when the flag is absent.
func (s EtcdStatus) EffectiveQuotaBytes() int64 {
	if s.QuotaBytes > 0 {
		return s.QuotaBytes
	}
	return EtcdDefaultQuotaBytes
}

// FragmentationRatio is the share of the backend file that is dead space - exactly what
// defragmentation reclaims. 0 when the store is empty or unobserved.
func (s EtcdStatus) FragmentationRatio() float64 {
	if s.DBBytes <= 0 || s.DBInUseBytes >= s.DBBytes {
		return 0
	}
	return float64(s.DBBytes-s.DBInUseBytes) / float64(s.DBBytes)
}

// QuotaUsage is how much of the enforced quota the backend file occupies, 0..1+. At 1.0 etcd arms
// NOSPACE and the cluster goes read-only.
func (s EtcdStatus) QuotaUsage() float64 {
	return float64(s.DBBytes) / float64(s.EffectiveQuotaBytes())
}

// HasAlarm reports whether the named alarm is armed.
func (s EtcdStatus) HasAlarm(name string) bool {
	for _, a := range s.Alarms {
		if a == name {
			return true
		}
	}
	return false
}

// EtcdDefragPolicy decides WHEN a cluster's etcd is defragmented. It is a plain value so the whole
// decision is unit-testable without a cluster, an ansible run, or a clock.
//
// The thresholds are deliberately OpenShift's etcd-defrag-controller numbers rather than invented
// ones: 45% fragmentation over a 100MiB floor. The floor is the important half - a 4MB store is
// routinely "200% fragmented" in ratio terms, and without an absolute minimum the platform would
// take a stop-the-world outage on every idle cluster forever to reclaim a few megabytes.
type EtcdDefragPolicy struct {
	// Enabled turns the whole feature - observation included - on. Off means the reconciler never
	// reads a cluster's etcd and never promotes into PhaseDefragmentingEtcd.
	Enabled bool
	// ObserveInterval is how often a Ready cluster's etcd status is re-read. Unlike certificate
	// expiry (observed once and then known), a backend store drifts, so this is a real cadence.
	ObserveInterval time.Duration
	// MinRatio is the fragmentation share at or above which defragmentation is worth its outage.
	MinRatio float64
	// MinBytes is the absolute backend-file size below which fragmentation is ignored entirely.
	MinBytes int64
	// MinInterval is the hysteresis floor between two defragmentations of the same cluster.
	MinInterval time.Duration
	// Window is when non-urgent defragmentation may run. The zero window allows any time.
	Window MaintenanceWindow
}

// DefragDue reports whether the cluster's etcd should be defragmented now, and why. The reason is
// returned for both answers so the caller can emit it: "why not" is the interesting half when an
// operator is asking why their fragmented cluster has not been touched.
//
// members is the cluster's control-plane count. A status read that saw FEWER members than that means
// one is unreachable, and defragmenting is refused outright: defrag blocks the member it runs on, so
// doing it while another member is already down is how a three-node cluster loses quorum. This is the
// single most important guard here - every other condition only decides whether the work is worth
// doing, this one decides whether it is safe.
func (p EtcdDefragPolicy) DefragDue(s EtcdStatus, members int, now time.Time) (bool, string) {
	if !p.Enabled {
		return false, "etcd maintenance disabled"
	}
	if s.ObservedAt.IsZero() {
		return false, "etcd status not observed yet"
	}
	if s.Members < members {
		return false, fmt.Sprintf("only %d of %d etcd members reachable - refusing to defragment", s.Members, members)
	}
	// An armed NOSPACE alarm means the cluster is ALREADY read-only. Defragmenting (and disarming)
	// is the only thing that restores writes, so it outranks the hysteresis floor and the maintenance
	// window alike - this is outage recovery, not hygiene.
	if s.HasAlarm(EtcdAlarmNoSpace) {
		return true, "etcd NOSPACE alarm armed - cluster is read-only, defragmenting now"
	}
	if s.DBBytes < p.MinBytes {
		return false, fmt.Sprintf("etcd store is %s, below the %s floor", HumanBytes(s.DBBytes), HumanBytes(p.MinBytes))
	}
	ratio := s.FragmentationRatio()
	if ratio < p.MinRatio {
		return false, fmt.Sprintf("etcd store is %.0f%% fragmented, below the %.0f%% threshold", ratio*100, p.MinRatio*100)
	}
	if s.DefraggedAt != nil && now.Sub(*s.DefraggedAt) < p.MinInterval {
		return false, fmt.Sprintf("etcd defragmented %s ago, within the %s minimum interval", now.Sub(*s.DefraggedAt).Truncate(time.Minute), p.MinInterval)
	}
	if !p.Window.Allows(now) {
		return false, fmt.Sprintf("etcd store is %.0f%% fragmented but outside the maintenance window (%s)", ratio*100, p.Window)
	}
	return true, fmt.Sprintf("etcd store is %.0f%% fragmented (%s of %s reclaimable)",
		ratio*100, HumanBytes(s.DBBytes-s.DBInUseBytes), HumanBytes(s.DBBytes))
}

// ObservationDue reports whether the cluster's etcd status is stale enough to re-read. A cluster
// that has never been observed is always due - the one-time backfill for clusters predating this
// feature, the same signal CertNotAfter == nil carries.
func (p EtcdDefragPolicy) ObservationDue(s *EtcdStatus, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	return s == nil || s.ObservedAt.IsZero() || now.Sub(s.ObservedAt) >= p.ObserveInterval
}

// EtcdMaintenanceDue reports whether the reconciler has etcd work for this cluster now - either a
// stale observation or an armed alarm. Narrow in the same way CertRenewalDue is: only a Ready,
// converged cluster qualifies, so etcd maintenance never races an in-flight transition (an upgrade
// restarts every control plane anyway, and a defrag mid-upgrade is asking for it).
func (c *Cluster) EtcdMaintenanceDue(p EtcdDefragPolicy, now time.Time) bool {
	if c.Phase != PhaseReady || c.ObservedGeneration != c.Generation {
		return false
	}
	return p.ObservationDue(c.Etcd, now)
}

// HumanBytes renders a byte count the way an operator reads it, for events and health summaries.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
