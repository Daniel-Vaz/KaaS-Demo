package ansible

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// EtcdStatus runs etcd-status.yml - a read-only `etcdctl endpoint status --cluster` + `alarm list`
// from the etcd pod on the first control plane - and parses what it wrote back to the artifacts dir.
func (m *Manager) EtcdStatus(ctx context.Context, c *domain.Cluster) (domain.EtcdStatus, error) {
	art, err := m.prep(c)
	if err != nil {
		return domain.EtcdStatus{}, err
	}
	if err := m.playbook(ctx, c, "etcd-status.yml", map[string]any{"artifacts_dir": art}); err != nil {
		return domain.EtcdStatus{}, err
	}
	return readEtcdStatus(art)
}

// DefragEtcd runs etcd-defrag.yml: a health pre-flight, then one member at a time, then a fresh
// observation. minRatio is the per-member re-check that makes a retried run resume rather than
// re-bounce members an earlier attempt already defragmented.
func (m *Manager) DefragEtcd(ctx context.Context, c *domain.Cluster, minRatio float64) (domain.EtcdStatus, error) {
	art, err := m.prep(c)
	if err != nil {
		return domain.EtcdStatus{}, err
	}
	extra := map[string]any{
		"artifacts_dir":         art,
		"etcd_defrag_min_ratio": minRatio,
	}
	if err := m.playbook(ctx, c, "etcd-defrag.yml", extra); err != nil {
		return domain.EtcdStatus{}, err
	}
	st, err := readEtcdStatus(art)
	if err != nil {
		return domain.EtcdStatus{}, err
	}
	now := time.Now().UTC()
	st.DefraggedAt = &now
	return st, nil
}

// endpointStatus mirrors the shape of `etcdctl endpoint status -w json`: an array with one entry per
// member. Only the fields the platform reasons about are decoded - notably NOT the member ids, which
// are uint64s that would lose precision decoded into the float64 encoding/json defaults to.
type endpointStatus struct {
	Endpoint string `json:"Endpoint"`
	Status   struct {
		DBSize      int64 `json:"dbSize"`
		DBSizeInUse int64 `json:"dbSizeInUse"`
		// DBSizeQuota is the quota the member is ACTUALLY enforcing, reported by etcd 3.6+. Absent
		// (0) on 3.5, where the manifest grep is the only source. Preferred when present because it
		// is what etcd enforces rather than what a file says - a hand-edited manifest whose etcd has
		// not restarted, or a flag etcd rejected, would make the two disagree, and only this one
		// decides when the cluster goes read-only.
		DBSizeQuota int64    `json:"dbSizeQuota"`
		Errors      []string `json:"errors"`
	} `json:"Status"`
}

// readEtcdStatus assembles a domain.EtcdStatus from the three files the status.yml tasks left in the
// artifacts dir.
func readEtcdStatus(art string) (domain.EtcdStatus, error) {
	raw, err := os.ReadFile(filepath.Join(art, "etcd-status.json"))
	if err != nil {
		return domain.EtcdStatus{}, fmt.Errorf("ansible: read etcd status: %w", err)
	}
	var members []endpointStatus
	if err := json.Unmarshal(raw, &members); err != nil {
		return domain.EtcdStatus{}, fmt.Errorf("ansible: parse etcd status: %w", err)
	}
	if len(members) == 0 {
		// Not an empty result to be stamped: a status read that reached no member at all tells us
		// nothing, and stamping it would both hide the problem and (because Members would be 0) look
		// exactly like the "a member is down" state the defrag guard keys on.
		return domain.EtcdStatus{}, fmt.Errorf("ansible: etcd status reported no members")
	}

	st := domain.EtcdStatus{Members: len(members), ObservedAt: time.Now().UTC()}
	// The cluster's numbers are the LARGEST member's. Members of a healthy etcd cluster track each
	// other closely (they apply the same raft log), so this is rarely a meaningful choice - but when
	// they do diverge, the biggest file is both the closest to the quota and the one whose
	// defragmentation reclaims the most, so it is the one worth deciding on.
	for _, mem := range members {
		if mem.Status.DBSize > st.DBBytes {
			st.DBBytes, st.DBInUseBytes = mem.Status.DBSize, mem.Status.DBSizeInUse
		}
		// The cluster's quota is the SMALLEST member's, not the chosen member's - a different
		// question from the sizes above, and it has a different right answer. etcd arms NOSPACE
		// per member, and one member arming it makes the WHOLE cluster read-only, so the lowest
		// ceiling anywhere is the one that decides. Pairing the largest size with the smallest
		// quota is the correct worst case for a headroom warning; on a uniformly-tuned cluster
		// (every one the platform builds) the two coincide.
		if q := mem.Status.DBSizeQuota; q > 0 && (st.QuotaBytes == 0 || q < st.QuotaBytes) {
			st.QuotaBytes = q
		}
		// etcd reports armed alarms per endpoint too; fold them in so an alarm is never missed
		// because `alarm list` was read from a member that had just been restarted.
		for _, e := range mem.Status.Errors {
			st.Alarms = appendAlarm(st.Alarms, e)
		}
	}

	if alarms, err := os.ReadFile(filepath.Join(art, "etcd-alarms")); err == nil {
		for _, line := range strings.Split(string(alarms), "\n") {
			st.Alarms = appendAlarm(st.Alarms, line)
		}
	}
	// Fall back to the quota grepped off the static-pod manifest only when etcd did not report one
	// itself - i.e. etcd 3.5, which has no dbSizeQuota field. Never an override: where both exist,
	// the running process is the authority over the file on disk.
	if st.QuotaBytes == 0 {
		if quota, err := os.ReadFile(filepath.Join(art, "etcd-quota")); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(quota)), 10, 64); err == nil {
				st.QuotaBytes = n
			}
		}
	}
	return st, nil
}

// appendAlarm normalizes one line of etcd alarm output into a domain alarm name and appends it if
// it's new. etcd names the same alarm in two places with different framing - `alarm list` prints
// "memberID:9 alarm:NOSPACE", endpoint status carries a per-endpoint error "alarm:NOSPACE" - so this
// matches on the alarm NAME appearing anywhere in the line rather than parsing either format
// exactly. Lines naming no known alarm (a transient per-endpoint error, a blank line) are dropped
// rather than surfaced as something the platform would then act on.
func appendAlarm(alarms []string, line string) []string {
	up := strings.ToUpper(line)
	for _, name := range []string{domain.EtcdAlarmNoSpace, domain.EtcdAlarmCorrupt} {
		if strings.Contains(up, name) && !slices.Contains(alarms, name) {
			alarms = append(alarms, name)
		}
	}
	return alarms
}
