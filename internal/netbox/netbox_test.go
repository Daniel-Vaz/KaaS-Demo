package netbox

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeNetBox is a minimal stand-in for the IPAM: it holds ip-address records keyed by address and
// records every request, so a test can assert on both the end state and the calls made.
type fakeNetBox struct {
	mu      sync.Mutex
	srv     *httptest.Server
	records map[string]map[string]any // address -> record
	nextID  int
	posts   int // POSTs to ip-addresses (creations)
	patches int // PATCHes (upserts of an existing address)
	deletes int
	tagged  bool // the kaas tag was created
	fail    bool // return 500 to every ip-address write
}

func newFakeNetBox(t *testing.T) *fakeNetBox {
	t.Helper()
	f := &fakeNetBox{records: map[string]map[string]any{}, nextID: 1}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/users/tokens/provision/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["username"] != "user" || body["password"] != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "provisioned-token"})
	})

	mux.HandleFunc("GET /api/extras/tags/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		count := 0
		if f.tagged {
			count = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": count})
	})
	mux.HandleFunc("POST /api/extras/tags/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tagged = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("GET /api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		// NetBox rejects a filter on a tag slug it doesn't know with a 400 - it does NOT return an
		// empty result. Reproduced faithfully, because that difference is a real bug we hit.
		if tag := r.URL.Query().Get("tag"); tag != "" && !f.tagged {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"tag":["Select a valid choice. ` + tag + ` is not one of the available choices."]}`))
			return
		}
		var results []map[string]any
		if addr := r.URL.Query().Get("address"); addr != "" {
			if rec, ok := f.records[addr]; ok {
				results = append(results, rec)
			}
		} else if needle := r.URL.Query().Get("description__ic"); needle != "" {
			for _, rec := range f.records {
				if strings.Contains(rec["description"].(string), needle) {
					results = append(results, rec)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(results), "results": results})
	})

	mux.HandleFunc("POST /api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["id"] = f.nextID
		f.nextID++
		f.posts++
		f.records[body["address"].(string)] = body
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("PATCH /api/ipam/ip-addresses/{id}/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.patches++
		body["id"] = f.records[body["address"].(string)]["id"]
		f.records[body["address"].(string)] = body
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("DELETE /api/ipam/ip-addresses/{id}/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deletes++
		for addr, rec := range f.records {
			if id, _ := rec["id"].(int); r.PathValue("id") == itoa(id) {
				delete(f.records, addr)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound) // already gone - the client must treat this as success
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func (f *fakeNetBox) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: f.srv.URL, Username: "user", Password: "pass", Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Registration must be an upsert keyed on the address: re-running a reconcile step (which happens
// on every retry and every re-provision) must converge, not pile up duplicate records.
func TestEnsureIPUpserts(t *testing.T) {
	f := newFakeNetBox(t)
	c := f.client(t)
	ctx := context.Background()

	rec := IPRecord{Address: "172.23.252.50/24", DNSName: "demo-cp-0", Description: Description("demo", "abc")}
	if err := c.EnsureIP(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec.DNSName = "demo-cp-0-renamed"
	if err := c.EnsureIP(ctx, rec); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || f.patches != 1 {
		t.Fatalf("posts=%d patches=%d, want one create then one in-place update", f.posts, f.patches)
	}
	if len(f.records) != 1 {
		t.Fatalf("records = %d, want 1 (the address is the key - a re-run must not duplicate it)", len(f.records))
	}
	if got := f.records["172.23.252.50/24"]["dns_name"]; got != "demo-cp-0-renamed" {
		t.Errorf("dns_name = %v, want the upsert to have converged the field", got)
	}
	if !f.tagged {
		t.Error("the kaas tag was not created - records would be unattributable and undeletable")
	}
}

// A token is provisioned from username/password when none is configured.
func TestTokenProvisioning(t *testing.T) {
	f := newFakeNetBox(t)
	c := f.client(t)
	if err := c.EnsureIP(context.Background(), IPRecord{Address: "172.23.252.51/24"}); err != nil {
		t.Fatal(err)
	}
	if c.token != "provisioned-token" {
		t.Fatalf("token = %q, want the one provisioned from username/password", c.token)
	}
}

// Deletion is scoped to OUR records (tag + cluster marker) and tolerates an already-gone record,
// so a retried teardown converges instead of failing forever.
func TestDeleteClusterIsScopedAndIdempotent(t *testing.T) {
	f := newFakeNetBox(t)
	c := f.client(t)
	ctx := context.Background()

	for _, ip := range []string{"172.23.252.50/24", "172.23.252.51/24"} {
		if err := c.EnsureIP(ctx, IPRecord{Address: ip, Description: Description("demo", "abc")}); err != nil {
			t.Fatal(err)
		}
	}
	// Another cluster's record, and a record owned by somebody else entirely.
	if err := c.EnsureIP(ctx, IPRecord{Address: "172.23.252.60/24", Description: Description("other", "xyz")}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.records["172.23.252.99/24"] = map[string]any{
		"id": 999, "address": "172.23.252.99/24", "description": "synced from vcenter",
	}
	f.mu.Unlock()

	if err := c.DeleteCluster(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	left := make([]string, 0, len(f.records))
	for addr := range f.records {
		left = append(left, addr)
	}
	f.mu.Unlock()
	if len(left) != 2 {
		t.Fatalf("records left = %v, want the other cluster's and the foreign one untouched", left)
	}

	// Re-running the teardown must be a no-op, not an error.
	if err := c.DeleteCluster(ctx, "abc"); err != nil {
		t.Fatalf("second DeleteCluster: %v, want idempotent success", err)
	}
}

// Regression: deleting a cluster that never registered anything, against a NetBox that has never
// seen us, must be a clean no-op. Previously the teardown filtered by our tag before the tag
// existed - NetBox 400s on an unknown tag slug rather than returning nothing - so the delete step
// failed and retried forever, leaving the cluster stuck in Deleting. This is the state of any
// cluster that fails while provisioning infrastructure.
func TestDeleteClusterBeforeTagExists(t *testing.T) {
	f := newFakeNetBox(t)
	c := f.client(t)

	if err := c.DeleteCluster(context.Background(), "51a2ef8f3d02ce2a"); err != nil {
		t.Fatalf("DeleteCluster against a NetBox with no kaas tag: %v - want a clean no-op", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tagged {
		t.Error("a teardown created our tag - a NetBox that has never seen us should be left untouched")
	}
	if f.deletes != 0 {
		t.Errorf("deletes = %d, want 0 (there was nothing of ours to release)", f.deletes)
	}
}

// The decorator registers every node address plus the HA VIP and the default LoadBalancer IP, and
// releases them on destroy.
func TestProvisionerRegistersAndReleases(t *testing.T) {
	f := newFakeNetBox(t)
	inner := provision.NewFake()
	p := Wrap(inner, f.client(t), nil, discardLog())
	ctx := context.Background()

	net := provision.NetworkSpec{
		CIDR: "172.23.252.0/24", Mode: "dhcp", Name: "serviceVMNetwork",
		VIP: "172.23.252.240", LoadBalancerIP: "172.23.252.230", ClusterName: "demo",
	}
	specs := []provision.NodeSpec{
		{VMName: "demo-cp-0", IP: "172.23.252.50"},
		{VMName: "demo-w-0", IP: "172.23.252.51"},
	}
	if _, err := p.EnsureNodes(ctx, "abc", net, specs); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	got := len(f.records)
	vip := f.records["172.23.252.240/24"]
	lb := f.records["172.23.252.230/24"]
	f.mu.Unlock()
	if got != 4 {
		t.Fatalf("records = %d, want 4 (two nodes + the VIP + the LoadBalancer IP)", got)
	}
	if vip == nil || vip["role"] != "vip" {
		t.Errorf("VIP record = %v, want it registered with role=vip", vip)
	}
	if lb == nil || lb["dns_name"] != "demo-gateway-lb" {
		t.Errorf("LoadBalancer record = %v, want it registered as demo-gateway-lb", lb)
	}
	if lb["role"] != "vip" {
		t.Errorf("LoadBalancer record = %v, want it registered with role=vip", lb)
	}

	if err := p.DestroyCluster(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) != 0 {
		t.Fatalf("records after destroy = %d, want 0 - the addresses would stay reserved forever", len(f.records))
	}
}

// A NetBox failure must FAIL the reconcile step (which then retries), not be swallowed: the phase
// would advance, nothing would re-trigger registration, and the IPAM would silently drift from
// reality - the exact condition that hands one address to two machines.
func TestNetBoxFailureFailsTheStepAndRetryConverges(t *testing.T) {
	f := newFakeNetBox(t)
	f.fail = true
	inner := provision.NewFake()
	p := Wrap(inner, f.client(t), nil, discardLog())
	ctx := context.Background()

	net := provision.NetworkSpec{CIDR: "172.23.252.0/24", ClusterName: "demo"}
	specs := []provision.NodeSpec{{VMName: "demo-cp-0", IP: "172.23.252.50"}}

	if _, err := p.EnsureNodes(ctx, "abc", net, specs); err == nil {
		t.Fatal("EnsureNodes with NetBox down = nil error, want the step to fail so it is retried")
	}

	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()

	if _, err := p.EnsureNodes(ctx, "abc", net, specs); err != nil {
		t.Fatalf("retry after NetBox recovered: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) != 1 {
		t.Fatalf("records after the retry = %d, want the node registered (the loop must converge)", len(f.records))
	}
}

// Wrapping must preserve EVERY optional capability the inner backend has - the reconciler resolves
// them through provision.As* before a rolling OS replacement or an auto-repair rung, and losing one
// silently drops a preflight (real regression: a NetBox-wrapped vSphere backend that had lost its
// NodeReplacer left an auto-repaired node drained but never rebuilt).
func TestWrapPreservesCapabilities(t *testing.T) {
	f := newFakeNetBox(t)
	// checkerFake has ImageChecker (real backends' shape); provision.Fake already has NodeReplacer and
	// NodePowerer, so the composite exercises all three optional capabilities through the wrapper.
	wrapped := Wrap(&checkerFake{Fake: provision.NewFake()}, f.client(t), nil, discardLog())

	ck, ok := provision.AsImageChecker(wrapped)
	if !ok {
		t.Fatal("the decorated provisioner lost its ImageChecker capability")
	}
	if err := ck.ImageAvailable("ubuntu-24.04-k8s-1.36.2"); err == nil {
		t.Error("ImageAvailable was not forwarded to the inner provisioner")
	}
	if _, ok := provision.AsNodeReplacer(wrapped); !ok {
		t.Error("the decorated provisioner lost its NodeReplacer capability")
	}
	if _, ok := provision.AsNodePowerer(wrapped); !ok {
		t.Error("the decorated provisioner lost its NodePowerer capability")
	}

	// An inner provisioner WITHOUT a capability must not gain a bogus one through the wrapper.
	plain := Wrap(&noReplaceFake{}, f.client(t), nil, discardLog())
	if _, ok := provision.AsImageChecker(plain); ok {
		t.Error("wrapping a provisioner with no ImageChecker must not synthesize one")
	}
	if _, ok := provision.AsNodeReplacer(plain); ok {
		t.Error("wrapping a provisioner with no NodeReplacer must not synthesize one")
	}
}

// noReplaceFake is a minimal Provisioner with NONE of the optional capabilities, to prove the
// wrapper never invents one.
type noReplaceFake struct{}

func (noReplaceFake) EnsureNodes(context.Context, string, provision.NetworkSpec, []provision.NodeSpec) ([]provision.ProvisionedNode, error) {
	return nil, nil
}
func (noReplaceFake) DestroyCluster(context.Context, string) error  { return nil }
func (noReplaceFake) ListManaged(context.Context) ([]string, error) { return nil, nil }

type checkerFake struct{ *provision.Fake }

func (c *checkerFake) ImageAvailable(name string) error {
	return errNotBuilt
}

var errNotBuilt = errStr("template not built")

type errStr string

func (e errStr) Error() string { return string(e) }
