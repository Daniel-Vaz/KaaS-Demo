package ansible

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// writeArtifacts lays down the three files the etcd_maintenance role's status.yml leaves in the
// artifacts dir, so the parser is exercised against exactly the shape it reads in production.
func writeArtifacts(t *testing.T, status, alarms, quota string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"etcd-status.json": status,
		"etcd-alarms":      alarms,
		"etcd-quota":       quota,
	} {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A real three-member `etcdctl endpoint status --cluster -w json`, trimmed to the fields the parser
// decodes. Note the member ids: uint64s well past 2^53, which is why nothing here decodes them.
const threeMemberStatus = `[
  {"Endpoint":"https://10.0.0.11:2379","Status":{"header":{"cluster_id":14841639068965178418,"member_id":10276657743932975437,"revision":8221,"raft_term":3},"version":"3.5.16","dbSize":209715200,"leader":10276657743932975437,"raftIndex":9001,"errors":[],"dbSizeInUse":104857600,"isLearner":false}},
  {"Endpoint":"https://10.0.0.12:2379","Status":{"header":{"cluster_id":14841639068965178418,"member_id":18305464768544219135,"revision":8221,"raft_term":3},"version":"3.5.16","dbSize":524288000,"leader":10276657743932975437,"raftIndex":9001,"errors":[],"dbSizeInUse":209715200,"isLearner":false}},
  {"Endpoint":"https://10.0.0.13:2379","Status":{"header":{"cluster_id":14841639068965178418,"member_id":12345678901234567890,"revision":8221,"raft_term":3},"version":"3.5.16","dbSize":167772160,"leader":10276657743932975437,"raftIndex":9001,"errors":[],"dbSizeInUse":104857600,"isLearner":false}}
]`

// TestReadEtcdStatusPicksLargestMember: the cluster's numbers are the largest member's, and the
// in-use figure must come from that SAME member - mixing the max of one with the max of another
// would report a fragmentation ratio no member actually has.
func TestReadEtcdStatusPicksLargestMember(t *testing.T) {
	dir := writeArtifacts(t, threeMemberStatus, "", "8589934592\n")
	st, err := readEtcdStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.DBBytes != 524288000 || st.DBInUseBytes != 209715200 {
		t.Fatalf("sizes = %d/%d, want the 500MiB member's 524288000/209715200", st.DBBytes, st.DBInUseBytes)
	}
	if st.Members != 3 {
		t.Fatalf("Members = %d, want 3", st.Members)
	}
	if st.QuotaBytes != 8589934592 {
		t.Fatalf("QuotaBytes = %d, want 8589934592", st.QuotaBytes)
	}
	if len(st.Alarms) != 0 {
		t.Fatalf("Alarms = %v, want none", st.Alarms)
	}
	if st.ObservedAt.IsZero() {
		t.Fatal("ObservedAt not stamped")
	}
	if got := st.FragmentationRatio(); got < 0.59 || got > 0.61 {
		t.Fatalf("FragmentationRatio = %v, want ~0.60", got)
	}
}

// TestReadEtcdStatusPartialCluster: a member that doesn't answer is simply absent from the array.
// That lower count is what tells the reconciler it must not defragment right now, so it has to
// survive parsing rather than being normalized away.
func TestReadEtcdStatusPartialCluster(t *testing.T) {
	const oneMember = `[{"Endpoint":"https://10.0.0.11:2379","Status":{"dbSize":100,"dbSizeInUse":50}}]`
	st, err := readEtcdStatus(writeArtifacts(t, oneMember, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if st.Members != 1 {
		t.Fatalf("Members = %d, want 1", st.Members)
	}
}

// TestReadEtcdStatusNoMembers: a read that reached nobody tells us nothing. Stamping it would both
// hide the problem and look exactly like the "a member is down" state - so it is an error.
func TestReadEtcdStatusNoMembers(t *testing.T) {
	if _, err := readEtcdStatus(writeArtifacts(t, `[]`, "", "")); err == nil {
		t.Fatal("an empty member array parsed successfully")
	}
}

// TestReadEtcdStatusAlarms covers both places etcd names the same alarm, with different framing.
func TestReadEtcdStatusAlarms(t *testing.T) {
	dir := writeArtifacts(t, threeMemberStatus, "memberID:10276657743932975437 alarm:NOSPACE\n", "")
	st, err := readEtcdStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasAlarm(domain.EtcdAlarmNoSpace) {
		t.Fatalf("Alarms = %v, want NOSPACE from `alarm list`", st.Alarms)
	}

	// The same alarm reported as a per-endpoint error instead.
	const withError = `[{"Endpoint":"https://10.0.0.11:2379","Status":{"dbSize":100,"dbSizeInUse":50,"errors":["alarm:NOSPACE"]}}]`
	st, err = readEtcdStatus(writeArtifacts(t, withError, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasAlarm(domain.EtcdAlarmNoSpace) {
		t.Fatalf("Alarms = %v, want NOSPACE from the endpoint errors", st.Alarms)
	}

	// Reported in BOTH places at once must still yield one alarm, not two.
	dir = writeArtifacts(t, withError, "memberID:1 alarm:NOSPACE\n", "")
	st, err = readEtcdStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Alarms) != 1 {
		t.Fatalf("Alarms = %v, want exactly one entry", st.Alarms)
	}
}

// TestReadEtcdStatusUntunedQuota: neither source reports a quota - etcd 3.5 (no dbSizeQuota) on a
// member carrying no --quota-backend-bytes flag, i.e. running against etcd's stock 2GiB.
func TestReadEtcdStatusUntunedQuota(t *testing.T) {
	st, err := readEtcdStatus(writeArtifacts(t, threeMemberStatus, "", "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if st.QuotaBytes != 0 {
		t.Fatalf("QuotaBytes = %d, want 0 (flag absent)", st.QuotaBytes)
	}
	if got := st.EffectiveQuotaBytes(); got != domain.EtcdDefaultQuotaBytes {
		t.Fatalf("EffectiveQuotaBytes = %d, want etcd's 2GiB default", got)
	}
}

// VERBATIM output of a real etcd 3.6.8 single-member cluster (captured from a live vSphere cluster,
// unedited). Kept exactly as etcd emitted it - including the fields the parser ignores - so the
// decoder is pinned against reality rather than against a fixture written from the documentation.
// 3.6 is where `dbSizeQuota` appears; threeMemberStatus above is the 3.5 shape, which has no such
// field and is why the manifest fallback still exists.
const etcd36Status = `[{"Endpoint": "https://172.23.252.221:2379", "Status": {"header": {"cluster_id": 8426949807062248900, "member_id": 2163420708693696539, "revision": 8422, "raft_term": 3}, "version": "3.6.8", "dbSize": 23478272, "leader": 2163420708693696539, "raftIndex": 9513, "raftTerm": 3, "raftAppliedIndex": 9513, "dbSizeInUse": 23474176, "storageVersion": "3.6.0", "dbSizeQuota": 8589934592, "downgradeInfo": {}}}]`

// TestReadEtcdStatusPrefersReportedQuota: where etcd reports its own enforced quota, that wins over
// the manifest grep. The running process is the authority over the file on disk - a manifest edited
// but not yet picked up (or a flag etcd rejected) makes them disagree, and only the process decides
// when the cluster goes read-only.
func TestReadEtcdStatusPrefersReportedQuota(t *testing.T) {
	// The file deliberately disagrees, and deliberately loses.
	st, err := readEtcdStatus(writeArtifacts(t, etcd36Status, "", "2147483648\n"))
	if err != nil {
		t.Fatal(err)
	}
	if st.QuotaBytes != 8589934592 {
		t.Fatalf("QuotaBytes = %d, want etcd's reported 8589934592, not the manifest's 2147483648", st.QuotaBytes)
	}
	// And the rest of the real capture decodes as etcd meant it.
	if st.DBBytes != 23478272 || st.DBInUseBytes != 23474176 || st.Members != 1 {
		t.Fatalf("real capture decoded as db=%d inUse=%d members=%d", st.DBBytes, st.DBInUseBytes, st.Members)
	}
}

// TestReadEtcdStatusFallsBackToManifest: on etcd 3.5 there is no dbSizeQuota, so the grep is the
// only source and must still be honoured.
func TestReadEtcdStatusFallsBackToManifest(t *testing.T) {
	st, err := readEtcdStatus(writeArtifacts(t, threeMemberStatus, "", "8589934592\n"))
	if err != nil {
		t.Fatal(err)
	}
	if st.QuotaBytes != 8589934592 {
		t.Fatalf("QuotaBytes = %d, want the manifest's 8589934592", st.QuotaBytes)
	}
}

// TestReadEtcdStatusTakesSmallestQuota: unlike the sizes (largest member), the quota is the SMALLEST
// across members. etcd arms NOSPACE per member and one member arming it makes the whole cluster
// read-only, so the lowest ceiling anywhere is the one that decides - pairing the largest size with
// the smallest quota is the correct worst case for a headroom warning.
func TestReadEtcdStatusTakesSmallestQuota(t *testing.T) {
	const mixed = `[
	  {"Endpoint":"https://10.0.0.11:2379","Status":{"dbSize":100,"dbSizeInUse":50,"dbSizeQuota":8589934592}},
	  {"Endpoint":"https://10.0.0.12:2379","Status":{"dbSize":200,"dbSizeInUse":100,"dbSizeQuota":2147483648}}
	]`
	st, err := readEtcdStatus(writeArtifacts(t, mixed, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if st.QuotaBytes != 2147483648 {
		t.Fatalf("QuotaBytes = %d, want the smallest member's 2147483648", st.QuotaBytes)
	}
	if st.DBBytes != 200 {
		t.Fatalf("DBBytes = %d, want the largest member's 200", st.DBBytes)
	}
}

// TestReadEtcdStatusIgnoresNoise: lines that name no known alarm - a blank line, a transient
// per-endpoint error - must not surface as something the platform would then act on.
func TestReadEtcdStatusIgnoresNoise(t *testing.T) {
	const withNoise = `[{"Endpoint":"https://10.0.0.11:2379","Status":{"dbSize":100,"dbSizeInUse":50,"errors":["etcdserver: request timed out"]}}]`
	st, err := readEtcdStatus(writeArtifacts(t, withNoise, "\n\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Alarms) != 0 {
		t.Fatalf("Alarms = %v, want none", st.Alarms)
	}
}
