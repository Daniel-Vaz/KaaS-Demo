package kubectl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const kc = `apiVersion: v1
kind: Config
clusters:
- name: kaas
  cluster:
    server: https://10.200.3.10:6443
`

// stubKubectl writes a fake kubectl that prints the kubeconfig it was handed, so a test can see
// exactly what the real binary would have been pointed at.
func stubKubectl(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubectl")
	script := "#!/bin/sh\n[ \"$1\" = \"--kubeconfig\" ] && cat \"$2\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The kubeconfig kubectl actually runs with must carry the remote-KVM proxy - this is the whole
// mechanism by which the Workloads/Monitoring/Security seams reach a cluster behind a remote
// hypervisor, and it is invisible everywhere else (no flags, no env).
func TestLocalExecerRoutesKubectlThroughProxy(t *testing.T) {
	e := NewLocalExecer(stubKubectl(t), t.TempDir(), "socks5://127.0.0.1:1080")
	res, err := e.Run(context.Background(), []byte(kc), "c1", []string{"get", "pods"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Stdout), "proxy-url: socks5://127.0.0.1:1080") {
		t.Errorf("kubectl ran with an unproxied kubeconfig:\n%s", res.Stdout)
	}
	if !strings.Contains(string(res.Stdout), "server: https://10.200.3.10:6443") {
		t.Errorf("the API server address was not preserved:\n%s", res.Stdout)
	}
}

// The local-hypervisor default must hand kubectl the kubeconfig verbatim.
func TestLocalExecerNoProxyLeavesKubeconfigAlone(t *testing.T) {
	e := NewLocalExecer(stubKubectl(t), t.TempDir(), "")
	res, err := e.Run(context.Background(), []byte(kc), "c1", []string{"get", "pods"})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != kc {
		t.Errorf("kubeconfig was rewritten with no proxy configured:\n%s", res.Stdout)
	}
}
