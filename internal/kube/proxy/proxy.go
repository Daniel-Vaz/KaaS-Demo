// Package proxy is the API-side kubectl.Execer for real (worker) mode. The API can't reach the
// cluster API server itself, so every kubectl invocation is forwarded to the worker's exec agent
// (the same host-networked agent that serves the interactive shell): one-shot commands over an HTTP
// POST to /kube-exec, and `kubectl logs -f` over a WebSocket to /kube-logs. It reuses the shell
// agent's address and bearer token (KAAS_SHELL_AGENT_ADDR / shellToken).
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/execagent"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube/kubectl"
	"github.com/coder/websocket"
)

// Execer forwards kubectl invocations to an exec agent. Each invocation is self-contained (it
// carries its own kubeconfig), so with several agents configured any of them can serve any call:
// the Execer round-robins and fails over (see internal/execagent).
type Execer struct {
	pool  *execagent.Pool // exec agent addresses, e.g. host.containers.internal:8082
	token string          // shared bearer token
	log   *slog.Logger
}

// NewExecer builds the proxy Execer over the given exec-agent pool.
func NewExecer(pool *execagent.Pool, token string, log *slog.Logger) *Execer {
	if log == nil {
		log = slog.Default()
	}
	return &Execer{pool: pool, token: token, log: log}
}

func (e *Execer) Run(ctx context.Context, kc []byte, clusterID string, args []string) (kubectl.Result, error) {
	return e.RunInput(ctx, kc, clusterID, nil, args)
}

// RunInput is Run with stdin forwarded to the agent's kubectl (nil = none) - the input path for the
// per-user kubeconfig mint's `kubectl create -f -`. It satisfies the optional inputExecer the mint
// type-asserts (see kubectl.go). Like Run, each call is self-contained and any agent can serve it.
func (e *Execer) RunInput(ctx context.Context, kc []byte, clusterID string, stdin []byte, args []string) (kubectl.Result, error) {
	body, err := json.Marshal(kube.ExecRequest{ClusterID: clusterID, Kubeconfig: kc, Args: args, Stdin: stdin})
	if err != nil {
		return kubectl.Result{}, err
	}
	candidates := e.pool.Candidates()
	for i, addr := range candidates {
		res, err := e.runOne(ctx, addr, body)
		// Only an unreachable agent is worth retrying elsewhere: once one answered, the reply
		// (including a kubectl failure) is the answer, and re-running the command on a second agent
		// would be a second execution against the cluster.
		if err != nil && errors.Is(err, errUnreachable) && i < len(candidates)-1 {
			e.log.Warn("reach kube exec agent - trying the next", "addr", addr, "err", err)
			continue
		}
		return res, err
	}
	return kubectl.Result{}, fmt.Errorf("no exec agent configured")
}

// errUnreachable marks a transport failure (as opposed to an agent that answered with an error),
// which is the only case worth failing over to another agent.
var errUnreachable = errors.New("exec agent unreachable")

func (e *Execer) runOne(ctx context.Context, addr string, body []byte) (kubectl.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/kube-exec", bytes.NewReader(body))
	if err != nil {
		return kubectl.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return kubectl.Result{}, fmt.Errorf("cannot reach the cluster workloads backend at %s: %w: %w", addr, errUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return kubectl.Result{}, fmt.Errorf("kube exec agent: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var out kube.ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return kubectl.Result{}, fmt.Errorf("decode kube exec response: %w", err)
	}
	if out.Error != "" {
		return kubectl.Result{}, errors.New(out.Error)
	}
	return kubectl.Result{Stdout: out.Stdout, Stderr: out.Stderr, Code: out.Code}, nil
}

func (e *Execer) Stream(ctx context.Context, kc []byte, clusterID string, args []string, sink kube.LogSink) error {
	opts := &websocket.DialOptions{}
	if e.token != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + e.token}}
	}
	// A log stream is read-only and idempotent, so failing over to another agent is free.
	candidates := e.pool.Candidates()
	var conn *websocket.Conn
	var err error
	for _, addr := range candidates {
		if conn, _, err = websocket.Dial(ctx, "ws://"+addr+"/kube-logs", opts); err == nil {
			break
		}
		e.log.Warn("dial kube-logs agent", "addr", addr, "err", err)
	}
	if err != nil {
		return fmt.Errorf("cannot reach the cluster logs backend at %s: %w", strings.Join(candidates, ", "), err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(1 << 20)

	payload, err := json.Marshal(kube.LogsRequest{ClusterID: clusterID, Kubeconfig: kc, Args: args})
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return err
	}
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return nil // normal close (kubectl exited) or ctx cancelled
		}
		if typ == websocket.MessageText {
			var ctrl struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &ctrl) == nil && ctrl.Error != "" {
				return errors.New(ctrl.Error)
			}
			continue
		}
		if werr := sink.Write(data); werr != nil {
			return nil // browser gone
		}
	}
}
