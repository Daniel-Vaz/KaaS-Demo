// Package agent is the node-ssh agent: a small HTTP/WebSocket server the API dials to open an SSH
// session on a cluster VM. It runs on the dedicated, host-networked node-ssh sandbox
// (cmd/node-ssh-agent, deploy/Containerfile.nodessh, the `nodessh` compose service) - the only
// place that holds the platform's VM SSH key and can route to the cluster subnets
// (docs/networking.md).
//
// It serves exactly one endpoint, GET /node-ssh, and starts exactly one kind of process: `ssh` to a
// caller-named node IP (internal/nodessh/sshpty). That single-purpose shape is the sandbox's
// containment story - it holds a real credential, so unlike the shell agent it cannot rely on
// "there's nothing here to steal"; instead there is no path from a session to anything but ssh
// itself. See internal/nodessh.
//
// Auth is the shared bearer token (KAAS_NODE_SSH_TOKEN) the API presents; empty disables auth (dev
// only, warned). Distinct from KAAS_SHELL_TOKEN so the two sandboxes are independently credentialed.
package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/nodessh"
	"github.com/Daniel-Vaz/KaaS-demo/internal/nodessh/sshpty"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
	"github.com/coder/websocket"
)

// Agent serves node SSH sessions over HTTP/WebSocket.
type Agent struct {
	token  string
	runner *sshpty.Runner
	log    *slog.Logger
}

// New builds an agent. An empty token disables auth (dev/local only) - the caller should warn.
func New(token string, runner *sshpty.Runner, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	return &Agent{token: token, runner: runner, log: log}
}

// Serve runs the agent HTTP server on addr and blocks until ctx is cancelled (or it fails to
// listen).
func (a *Agent) Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /node-ssh", a.handleSSH)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	a.log.Info("node-ssh agent listening", "addr", addr, "auth", a.token != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *Agent) handleSSH(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx := r.Context()
	term := shell.NewConn(ctx, conn)

	isText, data, err := term.ReadMessage()
	if err != nil || !isText {
		shell.WriteError(term, "expected handshake")
		_ = conn.Close(websocket.StatusProtocolError, "no handshake")
		return
	}
	var hs nodessh.Handshake
	if err := json.Unmarshal(data, &hs); err != nil {
		shell.WriteError(term, "bad handshake")
		_ = conn.Close(websocket.StatusProtocolError, "bad handshake")
		return
	}

	a.log.Info("node ssh session started", "cluster", hs.ClusterID, "node", hs.VMName, "ip", hs.IP)
	// Runner.Serve re-validates hs.IP as a literal address before it ever becomes an argv element -
	// the defence that does not depend on the API having authored it (see sshpty and nodessh.Handshake).
	if err := a.runner.Serve(ctx, hs.ClusterID, hs.VMName, hs.IP, hs.Rows, hs.Cols, term); err != nil {
		a.log.Warn("node ssh session error", "cluster", hs.ClusterID, "node", hs.VMName, "err", err)
		shell.WriteError(term, err.Error())
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	a.log.Info("node ssh session ended", "cluster", hs.ClusterID, "node", hs.VMName)
}

func (a *Agent) authorized(r *http.Request) bool {
	if a.token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
}
