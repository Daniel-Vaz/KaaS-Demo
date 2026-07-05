package kvmhost

import (
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keyFile writes a throwaway private key and returns its path (FromEnv stats it).
func keyFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kvm-key")
	if err := os.WriteFile(p, []byte("not-a-real-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The default topology - no KAAS_KVM_HOST - must produce exactly the old behaviour: the local
// libvirt socket, no Ansible ProxyCommand, no kubeconfig proxy.
func TestLocalHostIsUnchangedBehaviour(t *testing.T) {
	h, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if h.Remote() {
		t.Fatal("no KAAS_KVM_HOST set, but the host reports remote")
	}
	if got := h.LibvirtURI(); got != "qemu:///system" {
		t.Errorf("LibvirtURI = %q, want the local socket", got)
	}
	if got := h.AnsibleSSHCommonArgs(); got != "" {
		t.Errorf("AnsibleSSHCommonArgs = %q, want empty (VMs are directly routable)", got)
	}
	if got := h.KubeProxyURL(); got != "" {
		t.Errorf("KubeProxyURL = %q, want empty (no tunnel)", got)
	}
}

func TestRemoteHostDerivations(t *testing.T) {
	key := keyFile(t)
	t.Setenv("KAAS_KVM_HOST", "kvm.example.internal")
	t.Setenv("KAAS_KVM_SSH_USER", "kaasops")
	t.Setenv("KAAS_KVM_SSH_PORT", "2222")
	t.Setenv("KAAS_KVM_SSH_KEY_FILE", key)

	h, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !h.Remote() {
		t.Fatal("KAAS_KVM_HOST set, but the host reports local")
	}

	uri := h.LibvirtURI()
	for _, want := range []string{
		"qemu+ssh://kaasops@kvm.example.internal:2222/system",
		"sshauth=privkey",
		"keyfile=" + key,
		"known_hosts_verify=ignore", // no known_hosts file configured
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("LibvirtURI = %q, missing %q", uri, want)
		}
	}

	// The ProxyCommand must carry its own -i: Ansible's IdentityFile is the cluster VM key, which
	// does not open the bastion.
	args := h.AnsibleSSHCommonArgs()
	for _, want := range []string{"ProxyCommand=", "ssh -W %h:%p", "-i " + key, "-p 2222", "kaasops@kvm.example.internal"} {
		if !strings.Contains(args, want) {
			t.Errorf("AnsibleSSHCommonArgs = %q, missing %q", args, want)
		}
	}

	if got := h.KubeProxyURL(); got != "socks5://"+DefaultSocksAddr {
		t.Errorf("KubeProxyURL = %q, want the default SOCKS listener", got)
	}
}

// An explicit KAAS_LIBVIRT_URI is the escape hatch for a properly configured qemu+tls endpoint, so
// it must win over the derived qemu+ssh URI - while the SSH-based data paths stay in force.
func TestExplicitLibvirtURIWins(t *testing.T) {
	t.Setenv("KAAS_KVM_HOST", "kvm.example.internal")
	t.Setenv("KAAS_KVM_SSH_KEY_FILE", keyFile(t))
	t.Setenv("KAAS_LIBVIRT_URI", "qemu+tls://kvm.example.internal/system")

	h, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got := h.LibvirtURI(); got != "qemu+tls://kvm.example.internal/system" {
		t.Errorf("LibvirtURI = %q, want the explicit override", got)
	}
	if h.KubeProxyURL() == "" {
		t.Error("KubeProxyURL is empty - the override must not disable the tunnel the VMs are behind")
	}
}

// A known_hosts file switches both derived paths from "trust anything" to real verification.
func TestKnownHostsFileEnablesVerification(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "known_hosts")
	t.Setenv("KAAS_KVM_HOST", "kvm.example.internal")
	t.Setenv("KAAS_KVM_SSH_KEY_FILE", keyFile(t))
	t.Setenv("KAAS_KVM_KNOWN_HOSTS_FILE", kh)

	h, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if uri := h.LibvirtURI(); !strings.Contains(uri, "knownhosts="+kh) || strings.Contains(uri, "known_hosts_verify=ignore") {
		t.Errorf("LibvirtURI = %q, want it to verify against %s", uri, kh)
	}
	if args := h.AnsibleSSHCommonArgs(); !strings.Contains(args, "StrictHostKeyChecking=yes") {
		t.Errorf("AnsibleSSHCommonArgs = %q, want strict host-key checking", args)
	}
}

// Remote without a key is a misconfiguration we can detect at startup - far better than every
// reconcile failing later with an opaque ssh error.
func TestRemoteRequiresKey(t *testing.T) {
	t.Setenv("KAAS_KVM_HOST", "kvm.example.internal")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "KAAS_KVM_SSH_KEY_FILE") {
		t.Fatalf("err = %v, want a missing-key failure", err)
	}
	t.Setenv("KAAS_KVM_SSH_KEY_FILE", filepath.Join(t.TempDir(), "absent"))
	if _, err := FromEnv(); err == nil {
		t.Fatal("a KAAS_KVM_SSH_KEY_FILE that does not exist was accepted")
	}
}

// stubSSH writes a fake `ssh` that binds the -D address and serves it until killed - the one
// behaviour of a real SOCKS tunnel that Start depends on - so the readiness wait and the supervisor
// can be exercised without a hypervisor.
func stubSSH(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh")
	script := `#!/bin/sh
addr=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-D" ]; then addr="$2"; fi
  shift
done
exec python3 -c '
import socket, sys
host, port = sys.argv[1].rsplit(":", 1)
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((host, int(port)))
s.listen(8)
while True:
    s.accept()[0].close()
' "$addr"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Start must not return until the SOCKS listener is actually accepting: a reconcile that raced the
// tunnel would fail its first kubectl with "connection refused" for no visible reason.
func TestStartWaitsForTunnelThenStops(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available for the ssh stub")
	}
	t.Setenv("KAAS_KVM_HOST", "kvm.example.internal")
	t.Setenv("KAAS_KVM_SSH_KEY_FILE", keyFile(t))
	t.Setenv("KAAS_KVM_SSH_BIN", stubSSH(t))
	t.Setenv("KAAS_KVM_SOCKS_ADDR", "127.0.0.1:11080")

	h, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	conn, err := net.DialTimeout("tcp", h.SocksAddr, time.Second)
	if err != nil {
		t.Fatalf("Start returned but the SOCKS listener is not up: %v", err)
	}
	conn.Close()
	h.Stop()
}
