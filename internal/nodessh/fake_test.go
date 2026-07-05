package nodessh

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func testCluster() *domain.Cluster {
	return &domain.Cluster{
		ID:   "abc123",
		Name: "demo",
		Nodes: []domain.Node{
			{VMName: "demo-cp-0", Role: domain.RoleControlPlane, IP: "10.0.0.10"},
			{VMName: "demo-default-0", Role: domain.RoleWorker, Pool: "default", IP: "10.0.0.11"},
		},
	}
}

func TestRenderCommand(t *testing.T) {
	c := testCluster()
	n := &c.Nodes[1] // demo-default-0
	cases := []struct {
		line     string
		contains []string
	}{
		{"hostname", []string{"demo-default-0"}},
		{"whoami", []string{"kaas"}},
		{"id", []string{"uid=1000(kaas)", "sudo"}},
		{"uname -a", []string{"Linux", "demo-default-0"}},
		{"df -h", []string{"Filesystem", "/dev/vda1"}},
		{"free -h", []string{"Mem:", "Swap:"}},
		{"systemctl status kubelet", []string{"kubelet", "active (running)"}},
		{"sudo systemctl is-active kubelet", []string{"active"}}, // passwordless sudo passes through
		{"help", []string{"hostname", "systemctl"}},
		{"vim /etc/hosts", []string{"not modeled"}},
	}
	for _, tc := range cases {
		out := renderCommand(c, n, tc.line)
		for _, want := range tc.contains {
			if !strings.Contains(out, want) {
				t.Errorf("renderCommand(%q): missing %q\n---\n%s", tc.line, want, out)
			}
		}
	}
}

func TestRenderCommandClearSentinel(t *testing.T) {
	c := testCluster()
	if got := renderCommand(c, &c.Nodes[0], "clear"); got != cmdClear {
		t.Fatalf("clear: got %q, want cmdClear sentinel", got)
	}
}

// scriptConn feeds queued binary messages then EOF and records everything written - the same shape
// as the shell package's session test helper.
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
func (c *scriptConn) WriteText([]byte) error     { return nil }
func (c *scriptConn) Close() error               { return nil }

// TestFakeSessionEchoAndRun drives the whole fake through the shared Emulator: banner, prompt, local
// echo, and a rendered command - proving the extraction wired up correctly for this seam too.
func TestFakeSessionEchoAndRun(t *testing.T) {
	c := testCluster()
	sc := &scriptConn{in: [][]byte{[]byte("hostname\r")}}
	if err := (&Fake{}).Serve(context.Background(), c, &c.Nodes[1], sc); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	got := sc.out.String()
	if !strings.Contains(got, "simulated SSH session") {
		t.Errorf("missing banner:\n%s", got)
	}
	if !strings.Contains(got, "kaas@demo-default-0") {
		t.Errorf("missing prompt:\n%s", got)
	}
	if !strings.Contains(got, "demo-default-0") {
		t.Errorf("hostname command did not run:\n%s", got)
	}
	if !strings.Contains(got, "\r\n") {
		t.Errorf("expected CRLF line endings")
	}
}
