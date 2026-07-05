// Package proxy is the API-side shell.Backend for real (worker) mode. The API can't reach the
// cluster API server itself, so it dials the worker's exec agent (over host.containers.internal),
// sends the session handshake, and then bridges frames between the browser terminal and the
// worker's PTY. After the handshake the API is a dumb byte pipe.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/execagent"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
	"github.com/coder/websocket"
)

// Backend dials an exec agent for each session. Sessions are independent - the agent holds no
// state across them - so with several agents configured it simply takes the next one (see
// internal/execagent) and falls over to another if that one won't answer.
type Backend struct {
	pool  *execagent.Pool // exec agent addresses, e.g. host.containers.internal:8082
	token string          // shared bearer token (KAAS_SHELL_TOKEN)
	log   *slog.Logger
}

// New builds the proxy backend over the given exec-agent pool.
func New(pool *execagent.Pool, token string, log *slog.Logger) *Backend {
	if log == nil {
		log = slog.Default()
	}
	return &Backend{pool: pool, token: token, log: log}
}

// Serve ignores readOnly: the kubeconfig it forwards is already the right one (admin or read-only
// viewer, chosen by the API), and the viewer's RBAC enforces read-only at the cluster API server -
// so a real PTY needs no client-side restriction.
func (b *Backend) Serve(ctx context.Context, c *domain.Cluster, kubeconfig []byte, _ bool, term shell.Conn) error {
	opts := &websocket.DialOptions{}
	if b.token != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + b.token}}
	}
	// Round-robin over the agents, failing over to the next when one won't answer. A session is
	// pinned to whichever agent picks it up (the PTY lives there), but nothing outlives it.
	candidates := b.pool.Candidates()
	var wconn *websocket.Conn
	var err error
	for _, addr := range candidates {
		if wconn, _, err = websocket.Dial(ctx, "ws://"+addr+"/exec", opts); err == nil {
			break
		}
		b.log.Warn("dial exec agent", "addr", addr, "err", err)
	}
	if err != nil {
		shell.WriteError(term, fmt.Sprintf("cannot reach the cluster shell backend at %s: %v",
			strings.Join(candidates, ", "), err))
		return err
	}
	defer func() { _ = wconn.Close(websocket.StatusNormalClosure, "") }()
	worker := shell.NewConn(ctx, wconn)

	hs := shell.Handshake{
		ClusterID:  c.ID,
		Name:       c.Name,
		K8sVersion: c.K8sVersion,
		Kubeconfig: kubeconfig,
		Rows:       shell.DefaultRows,
		Cols:       shell.DefaultCols,
	}
	payload, err := json.Marshal(hs)
	if err != nil {
		return err
	}
	if err := worker.WriteText(payload); err != nil {
		shell.WriteError(term, "handshake failed: "+err.Error())
		return err
	}
	// browser <-> worker: stdin/resize up, stdout/exit down. Shared with internal/nodessh/proxy,
	// which bridges the identical framing to its own agent.
	return shell.Bridge(term, worker)
}
