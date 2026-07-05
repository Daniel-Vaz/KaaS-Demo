package kubectl

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/audit"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube/kubectl"
)

func testCluster() *domain.Cluster {
	return &domain.Cluster{ID: "cl-abc123", Name: "demo", Phase: domain.PhaseReady}
}

// fakeExecer answers `get pods` with the configured apiserver pod names and `logs` with that pod's
// canned output, counting every invocation so a test can assert what the cache did or didn't re-run.
type fakeExecer struct {
	mu    sync.Mutex
	pods  []string
	logs  map[string]string // pod name -> log tail
	fail  map[string]bool   // pod name -> its `logs` call fails
	calls map[string]int    // "pods" / "logs:<pod>" -> invocations
	delay time.Duration     // artificial latency on a `logs` call
}

func newFakeExecer(pods ...string) *fakeExecer {
	return &fakeExecer{pods: pods, logs: map[string]string{}, fail: map[string]bool{}, calls: map[string]int{}}
}

func (f *fakeExecer) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeExecer) Run(_ context.Context, _ []byte, _ string, args []string) (kubectl.Result, error) {
	if len(args) > 0 && args[0] == "get" {
		f.mu.Lock()
		f.calls["pods"]++
		f.mu.Unlock()
		return kubectl.Result{Stdout: []byte(strings.Join(f.pods, " "))}, nil
	}
	pod := args[3] // logs -n kube-system <pod> --tail=N
	f.mu.Lock()
	f.calls["logs:"+pod]++
	failed, delay := f.fail[pod], f.delay
	out := f.logs[pod]
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if failed {
		return kubectl.Result{Code: 1, Stderr: "Error from server: pod not found"}, nil
	}
	return kubectl.Result{Stdout: []byte(out)}, nil
}

// auditLine renders one apiserver audit JSON line.
func auditLine(id, verb, user, resource, ns string, ts string) string {
	return fmt.Sprintf(`{"kind":"Event","apiVersion":"audit.k8s.io/v1","auditID":%q,"stage":"ResponseComplete",`+
		`"level":"Metadata","verb":%q,"user":{"username":%q},"objectRef":{"resource":%q,"namespace":%q,"name":"thing"},`+
		`"responseStatus":{"code":200},"stageTimestamp":%q}`, id, verb, user, resource, ns, ts)
}

// TestParseAuditLogSkipsNonAuditLines checks the parser picks audit events out of the apiserver's
// interleaved stdout: klog lines, blank lines and unrelated JSON objects are all skipped.
func TestParseAuditLogSkipsNonAuditLines(t *testing.T) {
	log := strings.Join([]string{
		`I0722 10:00:00.123456       1 trace.go:236] Trace[123]: "Get" url:/api/v1/pods`,
		"",
		`{"kind":"Status","apiVersion":"v1","status":"Failure"}`,
		auditLine("a1", "create", "alice", "deployments", "default", "2026-07-22T10:00:01Z"),
		`W0722 10:00:02.000000       1 warning.go:70] some warning`,
		`{"broken":`,
		auditLine("a2", "delete", "bob", "secrets", "kube-system", "2026-07-22T10:00:03Z"),
	}, "\n")

	got := parseAuditLog([]byte(log))
	if len(got) != 2 {
		t.Fatalf("parsed %d events, want 2: %+v", len(got), got)
	}
	if got[0].AuditID != "a1" || got[0].Verb != "create" || got[0].User != "alice" {
		t.Errorf("first event wrong: %+v", got[0])
	}
	if got[1].Resource.Type != "secrets" || got[1].Resource.Namespace != "kube-system" {
		t.Errorf("second event's resource wrong: %+v", got[1].Resource)
	}
}

// TestEventsMergesEveryControlPlane checks an HA read tails every apiserver pod and merges the
// results into one ordered page.
func TestEventsMergesEveryControlPlane(t *testing.T) {
	ex := newFakeExecer("kube-apiserver-cp-0", "kube-apiserver-cp-1")
	ex.logs["kube-apiserver-cp-0"] = auditLine("a1", "create", "alice", "pods", "default", "2026-07-22T10:00:01Z")
	ex.logs["kube-apiserver-cp-1"] = auditLine("a2", "delete", "bob", "pods", "default", "2026-07-22T10:00:09Z")

	page, err := New(ex).Events(context.Background(), testCluster(), nil, audit.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("got %d events, want both control planes' merged", len(page.Events))
	}
	if page.Events[0].AuditID != "a2" {
		t.Errorf("want newest-first across pods, got %q first", page.Events[0].AuditID)
	}
}

// TestEventsSkipsUnreachableControlPlane checks one failing apiserver doesn't fail the whole read -
// a control plane can be momentarily unreachable during a rolling replacement.
func TestEventsSkipsUnreachableControlPlane(t *testing.T) {
	ex := newFakeExecer("kube-apiserver-cp-0", "kube-apiserver-cp-1")
	ex.logs["kube-apiserver-cp-0"] = auditLine("a1", "create", "alice", "pods", "default", "2026-07-22T10:00:01Z")
	ex.fail["kube-apiserver-cp-1"] = true

	page, err := New(ex).Events(context.Background(), testCluster(), nil, audit.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].AuditID != "a1" {
		t.Fatalf("want the reachable apiserver's event, got %+v", page.Events)
	}
}

// TestEventsErrorsWhenEveryControlPlaneFails checks a total read failure is an error rather than an
// empty feed - an empty page would look like "nothing has happened" AND would poison the cache.
func TestEventsErrorsWhenEveryControlPlaneFails(t *testing.T) {
	ex := newFakeExecer("kube-apiserver-cp-0")
	ex.fail["kube-apiserver-cp-0"] = true

	q := New(ex)
	if _, err := q.Events(context.Background(), testCluster(), nil, audit.Query{}); err == nil {
		t.Fatal("want an error when no apiserver answered")
	}
	// Nothing was cached, so the next read retries rather than serving the failure for CacheTTL.
	if _, err := q.Events(context.Background(), testCluster(), nil, audit.Query{}); err == nil {
		t.Fatal("want an error on the retry too")
	}
	if n := ex.count("logs:kube-apiserver-cp-0"); n != 2 {
		t.Errorf("tailed %d times, want a real retry (2) - a failure must not be cached", n)
	}
}

// TestEventsCachesWindowAcrossQueries is the point of the cache: changing a filter (or a second
// viewer, or a keystroke) re-runs audit.Assemble over the cached window rather than re-tailing the
// apiserver, which is a multi-megabyte read.
func TestEventsCachesWindowAcrossQueries(t *testing.T) {
	ex := newFakeExecer("kube-apiserver-cp-0")
	ex.logs["kube-apiserver-cp-0"] = strings.Join([]string{
		auditLine("a1", "create", "alice", "pods", "default", "2026-07-22T10:00:01Z"),
		auditLine("a2", "delete", "bob", "secrets", "kube-system", "2026-07-22T10:00:02Z"),
	}, "\n")

	q := New(ex)
	ctx := context.Background()
	if _, err := q.Events(ctx, testCluster(), nil, audit.Query{}); err != nil {
		t.Fatal(err)
	}
	page, err := q.Events(ctx, testCluster(), nil, audit.Query{Verb: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if n := ex.count("logs:kube-apiserver-cp-0"); n != 1 {
		t.Errorf("tailed %d times, want 1 - the second query must be served from the cached window", n)
	}
	if len(page.Events) != 1 || page.Events[0].AuditID != "a2" {
		t.Fatalf("filter not applied over the cached window: %+v", page.Events)
	}
	// The window is unfiltered in the cache, so a widening query still sees everything.
	page, err = q.Events(ctx, testCluster(), nil, audit.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 {
		t.Errorf("got %d events after widening the filter, want the whole cached window", len(page.Events))
	}
}

// TestCacheExpires checks the window is genuinely refreshed once the TTL passes - the poll must see
// new events, or the feed would freeze.
func TestCacheExpires(t *testing.T) {
	ex := newFakeExecer("kube-apiserver-cp-0")
	ex.logs["kube-apiserver-cp-0"] = auditLine("a1", "create", "alice", "pods", "default", "2026-07-22T10:00:01Z")

	q := &Querier{ex: ex, cache: newCache(time.Millisecond)}
	ctx := context.Background()
	if _, err := q.Events(ctx, testCluster(), nil, audit.Query{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := q.Events(ctx, testCluster(), nil, audit.Query{}); err != nil {
		t.Fatal(err)
	}
	if n := ex.count("logs:kube-apiserver-cp-0"); n != 2 {
		t.Errorf("tailed %d times, want 2 - an expired window must be re-fetched", n)
	}
}

// TestCacheIsPerCluster checks one cluster's window never answers another's read.
func TestCacheIsPerCluster(t *testing.T) {
	ex := newFakeExecer("kube-apiserver-cp-0")
	ex.logs["kube-apiserver-cp-0"] = auditLine("a1", "create", "alice", "pods", "default", "2026-07-22T10:00:01Z")

	q := New(ex)
	ctx := context.Background()
	if _, err := q.Events(ctx, testCluster(), nil, audit.Query{}); err != nil {
		t.Fatal(err)
	}
	other := &domain.Cluster{ID: "cl-other", Name: "other", Phase: domain.PhaseReady}
	if _, err := q.Events(ctx, other, nil, audit.Query{}); err != nil {
		t.Fatal(err)
	}
	if n := ex.count("logs:kube-apiserver-cp-0"); n != 2 {
		t.Errorf("tailed %d times, want one fetch per cluster (2)", n)
	}
}

// TestConcurrentReadsFetchOnce checks the entry lock collapses a burst of concurrent readers of the
// same cluster (several open tabs, a poll landing on a filter change) into ONE tail.
func TestConcurrentReadsFetchOnce(t *testing.T) {
	ex := newFakeExecer("kube-apiserver-cp-0")
	ex.logs["kube-apiserver-cp-0"] = auditLine("a1", "create", "alice", "pods", "default", "2026-07-22T10:00:01Z")
	ex.delay = 20 * time.Millisecond

	q := New(ex)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := q.Events(context.Background(), testCluster(), nil, audit.Query{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := ex.count("logs:kube-apiserver-cp-0"); n != 1 {
		t.Errorf("tailed %d times, want 1 - concurrent readers must share one fetch", n)
	}
}

// TestStaleWindowSurvivesATransientFailure checks a momentary read failure serves the last good
// window rather than blanking the feed mid-poll.
func TestStaleWindowSurvivesATransientFailure(t *testing.T) {
	ex := newFakeExecer("kube-apiserver-cp-0")
	ex.logs["kube-apiserver-cp-0"] = auditLine("a1", "create", "alice", "pods", "default", "2026-07-22T10:00:01Z")

	q := &Querier{ex: ex, cache: newCache(time.Millisecond)}
	ctx := context.Background()
	if _, err := q.Events(ctx, testCluster(), nil, audit.Query{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	ex.mu.Lock()
	ex.fail["kube-apiserver-cp-0"] = true
	ex.mu.Unlock()

	page, err := q.Events(ctx, testCluster(), nil, audit.Query{})
	if err != nil {
		t.Fatalf("want the stale window, got error: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].AuditID != "a1" {
		t.Fatalf("want the last good window, got %+v", page.Events)
	}
}
