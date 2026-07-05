// Package nodessh is the in-browser NODE terminal seam: an SSH session as the `kaas` user on one
// cluster VM, opened from the Nodes tab. It is the sibling of internal/shell - same framing, same
// agent-behind-the-API topology - but it answers a different question. The Terminal tab gives you
// kubectl against the cluster's API server; this gives you a root shell on a single machine, which
// is what you need when the API server is exactly what isn't working.
//
// # Why it is a separate seam, a separate sandbox and a separate binary
//
// The shell sandbox is credential-free by design: it holds only bash+kubectl, and every request
// carries its own kubeconfig. Node SSH cannot work that way, because reaching a VM needs the
// platform's SSH private key (KAAS_SSH_PRIVATE_KEY_FILE) - the key cloud-init injects into every VM
// of every cluster. Three tempting placements are all unsafe:
//
//   - Mount the key into the `shell` sandbox. Its /exec endpoint is a real bash PTY in that same
//     container, so any Terminal-tab user could `cat` the key and own the whole fleet.
//   - Ship the key in the handshake, as the kubeconfig already rides the text channel. The PTY
//     runner writes session files to /work and every session runs as the same uid, so a concurrent
//     bash user reads the key straight off the tmpfs.
//   - Host it on the worker. That is precisely what the shell sandbox was carved out to prevent.
//
// So this seam gets its own sandbox (deploy/Containerfile.nodessh + the `nodessh` compose service;
// cmd/node-ssh-agent). It is a SEPARATE BINARY on purpose, not the shell agent behind a flag: the
// key-holding image must be structurally incapable of serving a bash PTY, so that no single
// misconfigured env var can ever put a shell in the container that holds the fleet key.
//
// Its containment story is therefore the inverse of the shell sandbox's, and the difference is the
// whole point. That sandbox is safe because it holds nothing; this one holds the key and is safe
// because THE ONLY PROCESS IT EVER STARTS IS `ssh` TO A CALLER-NAMED NODE IP - never a shell. The
// user's session IS the ssh client: if it fails to connect, the session ends and they land nowhere.
// See internal/nodessh/sshpty for the flags that hold that property up (-e none above all).
//
// Access is gated at the API on WRITE (authorizeClusterWrite), not view. That is not an escalation:
// a write-role actor already holds the cluster-admin kubeconfig, and a privileged pod is root on
// these same nodes - SSH-as-kaas reaches nothing they could not already reach, it just makes the
// obvious thing easy. A read-role group-mate gets a 403.
//
// Still stubbed (marked "production would…"): the fleet-wide static key itself (production would run
// an SSH CA and mint a short-lived certificate scoped to one node per session, bounding a leak in
// both time and blast radius); host-key verification (nodes are destroyed and rebuilt by rolling OS
// replacement, so TOFU would break constantly - production would pair the user CA with host
// certificates); and session recording (this emits open/close audit events only, not the PTY stream).
package nodessh

import (
	"context"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
)

// Backend serves one interactive SSH session to a single node over term (the browser side). Serve
// blocks until the session ends: the terminal closes, ssh exits, or ctx is cancelled.
//
// There is no readOnly parameter, unlike shell.Backend. The seam is write-gated at the API, and a
// root shell on a box has no meaningful read-only mode to simulate - the fake would be lying.
type Backend interface {
	Serve(ctx context.Context, c *domain.Cluster, n *domain.Node, term shell.Conn) error
}

// Handshake is the first text frame the API's proxy backend sends to the node-ssh agent.
//
// IP is authored by the API from the cluster's own node row - never by the browser, which sends only
// a VM name. The agent re-validates it as a literal IP anyway (see sshpty.Runner.Serve): it is about
// to become an argv element, and an unvalidated string there could smuggle in an ssh option such as
// -oProxyCommand=. ClusterID/VMName are cosmetic (logging and the session dir), so the agent stays
// stateless and needs no store access.
type Handshake struct {
	ClusterID string `json:"cluster_id"`
	VMName    string `json:"vm_name"`
	IP        string `json:"ip"`
	Rows      uint16 `json:"rows"`
	Cols      uint16 `json:"cols"`
}
