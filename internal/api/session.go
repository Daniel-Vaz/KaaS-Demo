package api

// The post-upgrade half of the three streaming terminal endpoints - the cluster shell, node SSH and
// workload logs - expressed against a shell.Conn instead of an http.ResponseWriter.
//
// They are split out of their handlers for one reason: the js/wasm demo build (cmd/demo-wasm) serves
// the API from inside the browser, where there is no socket to hijack and so no WebSocket upgrade to
// perform. It bridges the browser's terminal to a shell.Conn of its own and calls these directly.
// Keeping the session logic here rather than in the handlers means the demo runs the SAME
// not-Ready/kubeconfig gating, per-user credential minting and node-SSH auditing as production,
// instead of a parallel copy that would quietly drift. Nothing here is build-tagged; the real
// handlers are the primary caller.
//
// Authorization deliberately stays in the callers: it happens BEFORE the upgrade so an unauthorized
// request gets a clean HTTP status rather than an in-terminal message, and the caller is what knows
// how to report one. Each function therefore takes an already-authorized cluster (and node).
//
// Each returns the WebSocket close status and reason the session ended with, which the real handlers
// pass to conn.Close and the demo bridge ignores.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	"github.com/Daniel-Vaz/KaaS-demo/internal/nodessh"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
	"github.com/coder/websocket"
)

// ResolveActor loads the user named by the request's session cookie, or nil. It is what withAuth
// does for an ordinary route, exported for the demo bridge: a browser terminal has no HTTP request
// of its own, so cmd/demo-wasm synthesizes one from the WebSocket URL and resolves the actor here
// rather than reaching into the session cookie format itself.
func (s *Server) ResolveActor(r *http.Request) *domain.User { return s.resolveActor(r) }

// RunShellSession serves one interactive cluster-terminal session on term. c must already have
// passed the view-scoped access check.
func (s *Server) RunShellSession(ctx context.Context, actor *domain.User, c *domain.Cluster, term shell.Conn) (websocket.StatusCode, string) {
	if c.Phase != domain.PhaseReady {
		shell.WriteError(term, fmt.Sprintf("cluster %q is not Ready (phase %s) - the shell opens once it is Ready", c.Name, c.Phase))
		return websocket.StatusPolicyViolation, "not ready"
	}
	// The PTY runs as the actor's OWN per-user credential: a full-access actor gets a writer cert
	// (cluster-admin via RBAC), a read-role group-mate a reader cert (RBAC-limited to reads - list yes,
	// mutate no), so their kubectl acts as themselves. The readOnly flag is only needed by the fake
	// backend, which has no real API server to enforce RBAC.
	kc, readOnly, err := s.app.UserKubeconfig(ctx, actor, c.ID)
	if err != nil {
		shell.WriteError(term, "kubeconfig not available yet: "+err.Error())
		return websocket.StatusInternalError, "no kubeconfig"
	}
	if len(kc) == 0 {
		shell.WriteError(term, "kubeconfig for this cluster is not ready yet - reconnect in a moment")
		return websocket.StatusTryAgainLater, "kubeconfig empty"
	}
	if err := s.app.Shell.Serve(ctx, c, kc, readOnly, term); err != nil {
		s.log.Warn("shell session", "cluster", c.Name, "err", err)
	}
	return websocket.StatusNormalClosure, ""
}

// RunNodeSSHSession serves one SSH session to node n of cluster c on term. Both must already have
// passed the write-scoped access check (see App.NodeSSHTarget).
func (s *Server) RunNodeSSHSession(ctx context.Context, actor *domain.User, c *domain.Cluster, n *domain.Node, term shell.Conn) (websocket.StatusCode, string) {
	if n.IP == "" {
		shell.WriteError(term, fmt.Sprintf("node %q has no IP yet - it is still being provisioned; reconnect once it has an address", n.VMName))
		return websocket.StatusPolicyViolation, "no node ip"
	}

	s.app.AuditNodeSSH(c, actor, n, "opened")
	// Record the session in the Operations history and capture the commands typed during it: the
	// recorder tees term's keystrokes (see internal/nodessh), transparent to the session, and the op
	// is completed with the command list on close.
	opID := s.app.BeginNodeSSHOperation(actor, c, n)
	rec := nodessh.NewCommandRecorder(term)
	if err := s.app.NodeSSH.Serve(ctx, c, n, rec); err != nil {
		s.log.Warn("node ssh session", "cluster", c.Name, "node", n.VMName, "err", err)
	}
	s.app.EndNodeSSHOperation(opID, rec.Commands(), rec.Truncated())
	s.app.AuditNodeSSH(c, actor, n, "closed")
	return websocket.StatusNormalClosure, ""
}

// RunLogSession streams one workload's pod logs to term. clusterID must already have passed the
// view-scoped access check; WorkloadLogs re-checks it anyway, as every app method does.
func (s *Server) RunLogSession(ctx context.Context, actor *domain.User, clusterID string, ref kube.LogRef, term shell.Conn) (websocket.StatusCode, string) {
	if ref.Pod == "" {
		shell.WriteError(term, "a pod query parameter is required")
		return websocket.StatusPolicyViolation, "no pod"
	}
	if err := s.app.WorkloadLogs(ctx, actor, clusterID, ref, wsLogSink{term}); err != nil {
		shell.WriteError(term, err.Error())
	}
	return websocket.StatusNormalClosure, ""
}
