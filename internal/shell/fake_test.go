package shell

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func testCluster() *domain.Cluster {
	return &domain.Cluster{
		ID:         "abc123",
		Name:       "demo",
		Bundle:     "2026.1",
		K8sVersion: "1.36.2",
		OSImage:    "ubuntu-24.04",
		CNI:        "cilium",
		CreatedAt:  time.Now().Add(-12 * time.Minute),
		Nodes: []domain.Node{
			{VMName: "demo-cp-0", Role: domain.RoleControlPlane, IP: "192.168.122.10"},
			{VMName: "demo-w-0", Role: domain.RoleWorker, IP: "192.168.122.11"},
			{VMName: "demo-w-1", Role: domain.RoleWorker, IP: "192.168.122.12"},
		},
		Addons: []domain.Addon{{Name: "metrics-server", Version: "3.12.1", Phase: "installed"}},
	}
}

func TestRenderCommand(t *testing.T) {
	c := testCluster()
	cases := []struct {
		line     string
		contains []string
		absent   []string
	}{
		{"kubectl get nodes", []string{"demo-cp-0", "demo-w-1", "control-plane", "Ready", "v1.36.2", "ROLES"}, nil},
		{"kubectl get nodes -o wide", []string{"192.168.122.10", "INTERNAL-IP", "Ubuntu 24.04"}, nil},
		{"kubectl get no", []string{"demo-cp-0"}, nil}, // resource alias
		{"kubectl version", []string{"Server Version: v1.36.2", "Client Version"}, nil},
		{"kubectl get pods -A", []string{"kube-apiserver-demo-cp-0", "coredns-", "cilium-", "metrics-server", "kube-system"}, nil},
		{"kubectl get pods", []string{"No resources found in default namespace."}, []string{"kube-apiserver"}},
		{"kubectl get ns", []string{"kube-system", "default", "Active"}, nil},
		{"kubectl get svc -A", []string{"kubernetes", "kube-dns", "443/TCP"}, nil},
		{"kubectl cluster-info", []string{"control plane", "CoreDNS"}, nil},
		{"kubectl config current-context", []string{"kubernetes-admin@demo"}, nil},
		{"help", []string{"kubectl get nodes", "cluster-info"}, nil},
		{"kubectl get widgets", []string{"not modeled"}, nil},
		{"kubectl rollout status", []string{"not modeled"}, nil},
		{"ls -la", []string{"only kubectl is modeled"}, nil},
	}
	for _, tc := range cases {
		out := renderCommand(c, tc.line, false)
		for _, want := range tc.contains {
			if !strings.Contains(out, want) {
				t.Errorf("renderCommand(%q): output missing %q\n---\n%s", tc.line, want, out)
			}
		}
		for _, no := range tc.absent {
			if strings.Contains(out, no) {
				t.Errorf("renderCommand(%q): output should not contain %q\n---\n%s", tc.line, no, out)
			}
		}
	}
}

func TestRenderCommandClearSentinel(t *testing.T) {
	if got := renderCommand(testCluster(), "clear", false); got != cmdClear {
		t.Fatalf("clear: got %q, want cmdClear sentinel", got)
	}
}

// In read-only mode the fake shell simulates the viewer kubeconfig's RBAC: read verbs still work,
// but mutating verbs are refused with a Forbidden error.
func TestRenderCommandReadOnly(t *testing.T) {
	c := testCluster()

	// Reads succeed the same as a full session.
	if out := renderCommand(c, "kubectl get nodes", true); !strings.Contains(out, "demo-cp-0") {
		t.Errorf("read-only `kubectl get nodes` should still list nodes, got:\n%s", out)
	}

	// Every mutating verb is Forbidden.
	for _, verb := range []string{"delete", "apply", "scale", "edit", "drain", "exec", "run"} {
		out := renderCommand(c, "kubectl "+verb+" pods foo", true)
		if !strings.Contains(out, "Forbidden") || !strings.Contains(out, "read-only") {
			t.Errorf("read-only `kubectl %s` = %q, want a Forbidden/read-only error", verb, out)
		}
	}

	// The same mutating verb is NOT blocked by the read-only gate when the session is full-access
	// (it falls through to the normal "not modeled" path instead of a Forbidden error).
	if out := renderCommand(c, "kubectl delete pods foo", false); strings.Contains(out, "Forbidden") {
		t.Errorf("full-access `kubectl delete` should not be Forbidden by the read-only gate, got:\n%s", out)
	}
}

// scriptConn is a Conn that feeds queued binary messages then EOF, and records everything written.
type scriptConn struct {
	in  [][]byte
	idx int
	out strings.Builder
}

func (c *scriptConn) ReadMessage() (bool, []byte, error) {
	if c.idx >= len(c.in) {
		return false, nil, io.EOF
	}
	m := c.in[c.idx]
	c.idx++
	return false, m, nil
}
func (c *scriptConn) WriteBinary(b []byte) error { c.out.Write(b); return nil }
func (c *scriptConn) WriteText(b []byte) error   { return nil }
func (c *scriptConn) Close() error               { return nil }

func TestFakeSessionEchoAndRun(t *testing.T) {
	sc := &scriptConn{in: [][]byte{[]byte("kubectl version\r")}}
	s := &fakeSession{term: sc, c: testCluster()}
	if err := s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := sc.out.String()
	// Banner + prompt on connect.
	if !strings.Contains(got, "KaaS demo shell") {
		t.Errorf("missing banner:\n%s", got)
	}
	if !strings.Contains(got, "kaas@demo") {
		t.Errorf("missing prompt:\n%s", got)
	}
	// Local echo of typed characters.
	if !strings.Contains(got, "kubectl version") {
		t.Errorf("missing echoed input:\n%s", got)
	}
	// Command output rendered with CRLF line endings.
	if !strings.Contains(got, "Server Version: v1.36.2") {
		t.Errorf("missing command output:\n%s", got)
	}
	if !strings.Contains(got, "\r\n") {
		t.Errorf("expected CRLF line endings in output")
	}
}

func TestFakeSessionBackspace(t *testing.T) {
	// Type "kubectl versionX", backspace the X, then Enter - should still run `kubectl version`.
	sc := &scriptConn{in: [][]byte{[]byte("kubectl versionX\x7f\r")}}
	s := &fakeSession{term: sc, c: testCluster()}
	if err := s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(sc.out.String(), "Server Version: v1.36.2") {
		t.Errorf("backspace not applied; command did not run:\n%s", sc.out.String())
	}
}
