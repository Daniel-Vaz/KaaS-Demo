package reconcile

import (
	"context"
	"fmt"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
)

// dnsRecorder records what the reconciler publishes and withdraws, in order, so the tests can assert
// both the fact and the SEQUENCE (the wildcard must be gone before the address it points at is
// recycled).
type dnsRecorder struct {
	events  []string
	destroy *[]string // shared log with the provisioner, when one is wired
	failNow bool
}

func (d *dnsRecorder) EnsureCluster(_ context.Context, c *domain.Cluster) error {
	if d.failNow {
		return fmt.Errorf("dns unavailable")
	}
	d.log("publish " + c.AppsDomain + " -> " + c.LoadBalancerIP)
	return nil
}

func (d *dnsRecorder) ReleaseCluster(_ context.Context, c *domain.Cluster) error {
	d.log("release " + c.AppsDomain)
	return nil
}

func (d *dnsRecorder) log(s string) {
	d.events = append(d.events, s)
	if d.destroy != nil {
		*d.destroy = append(*d.destroy, s)
	}
}

// destroyRecorder is a provisioner that appends to the same log the dnsRecorder writes to, so the
// ordering between the DNS withdrawal and the infrastructure teardown is observable.
type destroyRecorder struct {
	*provision.Fake
	log *[]string
}

func (p *destroyRecorder) DestroyCluster(ctx context.Context, clusterID string) error {
	*p.log = append(*p.log, "destroy "+clusterID)
	return p.Fake.DestroyCluster(ctx, clusterID)
}

func dnsCluster(id, name string) *domain.Cluster {
	return &domain.Cluster{
		ID: id, Name: name, K8sVersion: "1.36.2", Size: "small", CNI: "cilium",
		Phase: domain.PhasePending, Generation: 1, LoadBalancerIP: "10.200.3.249",
		DNSDomain: name + ".kaas.example.internal", AppsDomain: "apps." + name + ".kaas.example.internal",
		Addons: []domain.Addon{
			{Name: "metallb", Version: "0.16.1", Phase: "pending"},
			{Name: "envoy-gateway", Version: "v1.8.2", Phase: "pending"},
		},
	}
}

// TestDNSPublishedOnceAfterGateway: a cluster with a domain and a wired gateway publishes its apps
// wildcard exactly once, and the DNSWired marker records it so later ticks don't re-publish.
func TestDNSPublishedOnceAfterGateway(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &dnsRecorder{}
	r.DNS = rec
	if err := st.CreateCluster(dnsCluster("d1", "dev")); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "d1")

	want := "publish apps.dev.kaas.example.internal -> 10.200.3.249"
	if len(rec.events) != 1 || rec.events[0] != want {
		t.Fatalf("dns events = %v, want exactly [%q]", rec.events, want)
	}
	got, _ := st.GetCluster("d1")
	if !got.DNSWired {
		t.Fatal("DNSWired not set - the record would be re-published every tick")
	}
}

// TestDNSNotPublishedWithoutGateway: without the gateway add-ons there is nothing answering on the
// reserved address, so the name is not published (it would resolve into a black hole).
func TestDNSNotPublishedWithoutGateway(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &dnsRecorder{}
	r.DNS = rec
	c := dnsCluster("d2", "nogw")
	c.Addons = nil
	if err := st.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "d2")
	if len(rec.events) != 0 {
		t.Fatalf("dns events = %v, want none without a wired gateway", rec.events)
	}
}

// TestDNSReleasedBeforeDestroy is the invariant that matters most: the wildcard must be withdrawn
// BEFORE the infrastructure goes away, because destroying the cluster returns its reserved address
// to the pool - and a record left pointing at it would send this tenant's users into whichever
// cluster picks the address up next.
func TestDNSReleasedBeforeDestroy(t *testing.T) {
	r, st := newTestReconciler(t)
	var log []string
	rec := &dnsRecorder{destroy: &log}
	r.DNS = rec
	r.Prov = &destroyRecorder{Fake: provision.NewFake(), log: &log}

	if err := st.CreateCluster(dnsCluster("d3", "doomed")); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "d3")

	c, _ := st.GetCluster("d3")
	c.Phase = domain.PhaseDeleting
	c.Generation++
	if err := st.UpdateCluster(c); err != nil {
		t.Fatal(err)
	}
	converge(t, r, st, "d3")

	if len(log) != 3 ||
		log[0] != "publish apps.doomed.kaas.example.internal -> 10.200.3.249" ||
		log[1] != "release apps.doomed.kaas.example.internal" ||
		log[2] != "destroy d3" {
		t.Fatalf("sequence = %v, want publish → release → destroy", log)
	}
}

// A DNS failure must FAIL the step rather than let the cluster reach Ready with an unpublished name:
// the loop is level-triggered, so failing is what makes it converge later. Once the server answers,
// the retry publishes and the cluster proceeds.
func TestDNSFailureRetries(t *testing.T) {
	r, st := newTestReconciler(t)
	rec := &dnsRecorder{failNow: true}
	r.DNS = rec
	if err := st.CreateCluster(dnsCluster("d4", "flaky")); err != nil {
		t.Fatal(err)
	}
	// Tick until the DNS step is reached and refuses.
	var failed bool
	for i := 0; i < 10 && !failed; i++ {
		c, err := st.GetCluster("d4")
		if err != nil {
			t.Fatal(err)
		}
		if !c.NeedsWork() {
			break
		}
		if err := r.reconcileOne(context.Background(), c); err != nil {
			failed = true
		}
	}
	if !failed {
		t.Fatal("a DNS failure did not fail the reconcile step")
	}
	if c, _ := st.GetCluster("d4"); c.Phase == domain.PhaseReady {
		t.Fatal("cluster reached Ready with its apps domain unpublished")
	}
	rec.failNow = false
	converge(t, r, st, "d4")
	if c, _ := st.GetCluster("d4"); !c.DNSWired {
		t.Fatal("retry did not publish the record")
	}
}
