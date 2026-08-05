// Package proxy is the API-side nodessh.Backend for real mode. The API has no network route to a
// cluster VM (docs/networking.md), so it dials the host-networked node-ssh sandbox, sends the
// session handshake, and then bridges frames between the browser terminal and the sandbox's ssh
// client. After the handshake the API is a dumb byte pipe.
//
// It is the twin of internal/shell/proxy, with one difference that matters: the handshake carries no
// credential. The shell's handshake ships the cluster's kubeconfig; here the SSH key lives in the
// sandbox and never crosses this hop - which is exactly why the sandbox has to be its own container
// (see internal/nodessh).
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/execagent"
	"github.com/Daniel-Vaz/KaaS-demo/internal/nodessh"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
	"github.com/coder/websocket"
)

// Backend dials a node-ssh agent for each session. Sessions are independent - the agent holds no
// state across them - so with several agents configured it takes the next one (see
// internal/execagent) and falls over to another if that one won't answer.
type Backend struct {
	pool  *execagent.Pool // node-ssh agent addresses, e.g. host.containers.internal:8084
	token string          // shared bearer token (KAAS_NODE_SSH_TOKEN)
	log   *slog.Logger
}

// New builds the proxy backend over the given node-ssh agent pool.
func New(pool *execagent.Pool, token string, log *slog.Logger) *Backend {
	if log == nil {
		log = slog.Default()
	}
	return &Backend{pool: pool, token: token, log: log}
}

// Serve opens one SSH session to node n of cluster c and pipes it to term.
func (b *Backend) Serve(ctx context.Context, c *domain.Cluster, n *domain.Node, term shell.Conn) error {
	opts := execagent.DialOptions(b.token)
	candidates := b.pool.Candidates()
	var wconn *websocket.Conn
	var err error
	for _, addr := range candidates {
		if wconn, _, err = websocket.Dial(ctx, "ws://"+addr+"/node-ssh", opts); err == nil {
			break
		}
		b.log.Warn("dial node-ssh agent", "addr", addr, "err", err)
	}
	if err != nil {
		shell.WriteError(term, fmt.Sprintf("cannot reach the node SSH backend at %s: %v",
			strings.Join(candidates, ", "), err))
		return err
	}
	defer func() { _ = wconn.Close(websocket.StatusNormalClosure, "") }()
	agent := shell.NewConn(ctx, wconn)

	// The IP is read from the node row here, on the API side, and never taken from the browser -
	// which names only a VM. The agent re-validates it regardless (see sshpty.Runner.Serve).
	payload, err := json.Marshal(nodessh.Handshake{
		ClusterID: c.ID,
		VMName:    n.VMName,
		IP:        n.IP,
		Rows:      shell.DefaultRows,
		Cols:      shell.DefaultCols,
	})
	if err != nil {
		return err
	}
	if err := agent.WriteText(payload); err != nil {
		shell.WriteError(term, "handshake failed: "+err.Error())
		return err
	}
	return shell.Bridge(term, agent)
}
