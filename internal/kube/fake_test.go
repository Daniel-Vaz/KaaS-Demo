package kube

import (
	"context"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func testCluster() *domain.Cluster {
	return &domain.Cluster{
		ID: "c1", Name: "demo", Size: "small", K8sVersion: "1.36.2",
		CNI: "cilium", CNIVersion: "1.16.1", PodCIDR: "10.244.0.0/16",
		CreatedAt: time.Now().Add(-30 * time.Minute),
		Nodes: []domain.Node{
			{Role: domain.RoleControlPlane, VMName: "demo-cp-0", IP: "10.0.0.10"},
			{Role: domain.RoleWorker, VMName: "demo-w-0", IP: "10.0.0.20"},
			{Role: domain.RoleWorker, VMName: "demo-w-1", IP: "10.0.0.21"},
		},
		Addons: []domain.Addon{{Name: "metrics-server", Phase: "installed"}},
	}
}

func find(ws []WorkloadSummary, kind WorkloadKind, name string) (WorkloadSummary, bool) {
	for _, w := range ws {
		if w.Kind == kind && w.Name == name {
			return w, true
		}
	}
	return WorkloadSummary{}, false
}

func TestFakeWorkloadsCoverAllKinds(t *testing.T) {
	f := NewFake()
	c := testCluster()
	ws, err := f.Workloads(context.Background(), c, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[WorkloadKind]bool{}
	for _, w := range ws {
		seen[w.Kind] = true
	}
	for _, k := range AllKinds {
		if !seen[k] {
			t.Errorf("no %s workload synthesized", k)
		}
	}
	// The web Deployment should carry its desired replicas and an image.
	web, ok := find(ws, KindDeployment, "web")
	if !ok {
		t.Fatal("web deployment missing")
	}
	if web.DesiredReplicas != 3 || web.ReadyReplicas != 3 {
		t.Errorf("web replicas = %d/%d, want 3/3", web.ReadyReplicas, web.DesiredReplicas)
	}
	if len(web.Images) == 0 {
		t.Error("web deployment has no images")
	}
	// A DaemonSet runs one pod per node.
	kp, ok := find(ws, KindDaemonSet, "kube-proxy")
	if !ok || kp.DesiredReplicas != len(c.Nodes) {
		t.Errorf("kube-proxy desired = %d, want %d", kp.DesiredReplicas, len(c.Nodes))
	}
}

func TestFakeNamespaceFilter(t *testing.T) {
	f := NewFake()
	c := testCluster()
	ws, err := f.Workloads(context.Background(), c, nil, "demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.Namespace != "demo" {
			t.Errorf("namespace filter leaked %s/%s", w.Namespace, w.Name)
		}
	}
	if _, ok := find(ws, KindDeployment, "web"); !ok {
		t.Error("expected web in demo namespace")
	}

	ns, err := f.Namespaces(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ns, "demo") || !contains(ns, "kube-system") {
		t.Errorf("namespaces = %v, want demo + kube-system", ns)
	}
}

func TestFakeScaleReflectsAndRespectsReadOnly(t *testing.T) {
	f := NewFake()
	c := testCluster()
	ref := WorkloadRef{Kind: KindDeployment, Namespace: "demo", Name: "web"}

	if err := f.Scale(context.Background(), c, nil, ref, 5, true); err == nil {
		t.Fatal("read-only scale should be refused")
	}
	if err := f.Scale(context.Background(), c, nil, ref, 5, false); err != nil {
		t.Fatalf("scale: %v", err)
	}
	ws, _ := f.Workloads(context.Background(), c, nil, "demo")
	web, _ := find(ws, KindDeployment, "web")
	if web.DesiredReplicas != 5 {
		t.Errorf("after scale, web desired = %d, want 5", web.DesiredReplicas)
	}
	d, err := f.Workload(context.Background(), c, nil, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Pods) != 5 {
		t.Errorf("web detail has %d pods, want 5", len(d.Pods))
	}

	// A DaemonSet is not scalable.
	if err := f.Scale(context.Background(), c, nil, WorkloadRef{Kind: KindDaemonSet, Namespace: "kube-system", Name: "kube-proxy"}, 2, false); err == nil {
		t.Error("scaling a DaemonSet should be refused")
	}
}

func TestFakeLogsTailAndFollow(t *testing.T) {
	f := NewFake()
	c := testCluster()
	ref := LogRef{Namespace: "demo", Pod: "web-abc-123", Container: "nginx", TailLines: 5}

	var s captureSink
	if err := f.Logs(context.Background(), c, nil, ref, &s); err != nil {
		t.Fatal(err)
	}
	if s.lines != 5 {
		t.Errorf("tail emitted %d lines, want 5", s.lines)
	}

	// Follow should keep emitting until ctx is cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	ref.Follow = true
	var s2 captureSink
	if err := f.Logs(ctx, c, nil, ref, &s2); err != nil {
		t.Fatal(err)
	}
	if s2.lines <= 5 {
		t.Errorf("follow emitted %d lines, want > 5 (tail + streamed)", s2.lines)
	}
}

type captureSink struct{ lines int }

func (s *captureSink) Write(p []byte) error { s.lines++; return nil }

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
