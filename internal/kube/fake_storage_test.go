package kube

import (
	"context"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func findPVCSummary(ps []PVCSummary, namespace, name string) (PVCSummary, bool) {
	for _, p := range ps {
		if p.Namespace == namespace && p.Name == name {
			return p, true
		}
	}
	return PVCSummary{}, false
}

func TestFakePVCsBoundAndPending(t *testing.T) {
	f := NewFake()
	c := testCluster()
	ps, err := f.PVCs(context.Background(), c, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// A bound claim reports a volume and its granted capacity.
	up, ok := findPVCSummary(ps, "demo", "uploads")
	if !ok {
		t.Fatal("uploads claim missing")
	}
	if up.Status != PVCPhaseBound {
		t.Errorf("uploads status = %q, want %q", up.Status, PVCPhaseBound)
	}
	if up.Volume == "" {
		t.Error("bound claim has no volume name")
	}
	if up.Capacity != "20Gi" {
		t.Errorf("uploads capacity = %q, want 20Gi", up.Capacity)
	}

	// A claim with no explicit capacity is granted what it requested.
	cache, ok := findPVCSummary(ps, "demo", "data-cache-0")
	if !ok {
		t.Fatal("data-cache-0 claim missing")
	}
	if cache.Capacity != cache.Requested {
		t.Errorf("data-cache-0 capacity = %q, want the requested %q", cache.Capacity, cache.Requested)
	}

	// The pending claim is the page's non-Bound case: no volume, no granted capacity.
	arch, ok := findPVCSummary(ps, "demo", "archive")
	if !ok {
		t.Fatal("archive claim missing")
	}
	if arch.Status != PVCPhasePending {
		t.Errorf("archive status = %q, want %q", arch.Status, PVCPhasePending)
	}
	if arch.Volume != "" || arch.Capacity != "" {
		t.Errorf("pending claim should have no volume/capacity, got %q/%q", arch.Volume, arch.Capacity)
	}
	if arch.Requested == "" {
		t.Error("pending claim should still report what it requested")
	}
}

func TestFakePVCNamespaceFilter(t *testing.T) {
	f := NewFake()
	c := testCluster()
	ps, err := f.PVCs(context.Background(), c, nil, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 {
		t.Fatal("no claims in the demo namespace")
	}
	for _, p := range ps {
		if p.Namespace != "demo" {
			t.Errorf("namespace filter leaked %s/%s", p.Namespace, p.Name)
		}
	}
}

// The monitoring stack's claims should appear only when its add-on is installed - the fake is
// synthesized from control-plane state, so an add-on-less cluster must not invent its volumes.
func TestFakePVCsFollowAddons(t *testing.T) {
	f := NewFake()
	c := testCluster()
	ps, _ := f.PVCs(context.Background(), c, nil, "")
	if _, ok := findPVCSummary(ps, monitoringNS, "kube-prometheus-stack-grafana"); ok {
		t.Error("grafana claim present without the kube-prometheus-stack add-on")
	}

	c.Addons = append(c.Addons, domainAddon("kube-prometheus-stack"))
	ps, _ = f.PVCs(context.Background(), c, nil, "")
	if _, ok := findPVCSummary(ps, monitoringNS, "kube-prometheus-stack-grafana"); !ok {
		t.Error("grafana claim missing with the kube-prometheus-stack add-on installed")
	}

	// The namespace picker is shared with the Storage page, so every namespace holding a claim must
	// be listed - otherwise the page shows claims the picker can't filter to.
	ns, _ := f.Namespaces(context.Background(), c, nil)
	for _, p := range ps {
		if !contains(ns, p.Namespace) {
			t.Errorf("claim namespace %q is not offered by Namespaces()", p.Namespace)
		}
	}
}

func TestFakePVCDetailBindsVolume(t *testing.T) {
	f := NewFake()
	c := testCluster()

	d, err := f.PVC(context.Background(), c, nil, PVCRef{Namespace: "demo", Name: "data-cache-0"})
	if err != nil {
		t.Fatal(err)
	}
	if d.PersistentVolume == nil {
		t.Fatal("bound claim has no persistent volume")
	}
	if d.PersistentVolume.Name != d.Volume {
		t.Errorf("detail PV name %q != summary volume %q", d.PersistentVolume.Name, d.Volume)
	}
	if len(d.UsedBy) == 0 {
		t.Error("data-cache-0 should be mounted by cache-0")
	}

	// A pending claim has no PV, and its events say why.
	p, err := f.PVC(context.Background(), c, nil, PVCRef{Namespace: "demo", Name: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if p.PersistentVolume != nil {
		t.Error("pending claim should have no persistent volume")
	}
	ev, err := f.PVCEvents(context.Background(), c, nil, PVCRef{Namespace: "demo", Name: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) == 0 {
		t.Fatal("pending claim has no events")
	}
	if ev[0].Reason != "WaitForFirstConsumer" {
		t.Errorf("pending claim event reason = %q, want WaitForFirstConsumer", ev[0].Reason)
	}

	if _, err := f.PVC(context.Background(), c, nil, PVCRef{Namespace: "demo", Name: "nope"}); err == nil {
		t.Error("an unknown claim should be an error")
	}
}

func TestFakeStorageClasses(t *testing.T) {
	f := NewFake()
	c := testCluster()
	scs, err := f.StorageClasses(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scs) < 2 {
		t.Fatalf("got %d storage classes, want at least 2", len(scs))
	}
	defaults := 0
	for _, sc := range scs {
		if sc.IsDefault {
			defaults++
		}
		if sc.Provisioner == "" {
			t.Errorf("storage class %q has no provisioner", sc.Name)
		}
	}
	// Exactly one default class - two would be a cluster misconfiguration, and the fake shouldn't
	// model an invalid cluster.
	if defaults != 1 {
		t.Errorf("got %d default storage classes, want exactly 1", defaults)
	}

	// Every claim must reference a class that exists, or the page renders dangling references.
	ps, _ := f.PVCs(context.Background(), c, nil, "")
	for _, p := range ps {
		found := false
		for _, sc := range scs {
			if sc.Name == p.StorageClass {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("claim %s/%s references unknown storage class %q", p.Namespace, p.Name, p.StorageClass)
		}
	}
}

func TestFakeStorageManifests(t *testing.T) {
	f := NewFake()
	c := testCluster()

	y, err := f.PVCManifest(context.Background(), c, nil, PVCRef{Namespace: "demo", Name: "uploads"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: PersistentVolumeClaim", "name: uploads", "ReadWriteMany", "phase: Bound"} {
		if !containsStr(y, want) {
			t.Errorf("claim YAML missing %q:\n%s", want, y)
		}
	}

	y, err = f.StorageClassManifest(context.Background(), c, nil, defaultFakeClass)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: StorageClass", "name: " + defaultFakeClass, "is-default-class"} {
		if !containsStr(y, want) {
			t.Errorf("class YAML missing %q:\n%s", want, y)
		}
	}

	if _, err := f.StorageClassManifest(context.Background(), c, nil, "nope"); err == nil {
		t.Error("an unknown storage class should be an error")
	}
}

// domainAddon builds an installed add-on, so a test can light up the storage an add-on brings.
func domainAddon(name string) domain.Addon {
	return domain.Addon{Name: name, Phase: "installed"}
}

func containsStr(haystack, needle string) bool { return strings.Contains(haystack, needle) }
