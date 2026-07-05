// Package shell is the in-browser cluster terminal seam. Once a cluster is Ready the portal opens
// a WebSocket to the API, which delegates to a Backend: the fake backend synthesizes kubectl output
// in-process (so make up-fake stays demoable with no KVM), while the worker-proxy backend bridges
// the session to a real bash+kubectl PTY running on the host-networked worker - the only place with
// a network route to the cluster's API server (see docs/networking.md). Both speak the same tiny
// framing over a Conn, so the session logic is transport-agnostic.
//
// Isolation: the real shell is a full bash+kubectl PTY, so it is confined by WHERE it runs, not by
// restricting what the user may type. The worker-proxy backend forwards each session to the
// dedicated shell-agent sandbox (deploy/Containerfile.shell + the `shell` compose service;
// cmd/shell-agent), a minimal, unprivileged, host-networked container that carries only bash+kubectl
// and none of the control plane's secrets, keys, sockets or DB access - so a user who escapes into
// arbitrary bash finds nothing confidential to read. The PTY additionally hands the child a scrubbed
// environment (internal/shell/pty) so process-env secrets never leak via `env`. Access is gated at
// the API (owner-or-admin-or-group-mate) and the API↔sandbox hop by a shared bearer token.
//
// Still stubbed (marked "production would…"): a per-session ephemeral pod rather than a shared
// sandbox; an RBAC-scoped kubeconfig for full-access users (today they get cluster-admin; read-role
// group-mates already get the RBAC-limited viewer kubeconfig); session audit; server-side session
// revocation; and TLS/mTLS on the API↔sandbox hop instead of a plaintext localhost token.
package shell

import (
	"context"
	"encoding/json"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/coder/websocket"
)

// Backend serves one interactive terminal session for a cluster over term (the browser side).
// Serve blocks until the session ends: the terminal is closed, the underlying shell exits, or ctx
// is cancelled. kubeconfig is the cluster's decrypted kubeconfig - the cluster-admin one for a
// full-access user, or the read-only viewer one for a read-role group-mate. readOnly reports which:
// the real (worker) backend ignores it because RBAC on the viewer kubeconfig enforces read-only
// server-side, but the fake backend uses it to simulate that enforcement (it has no API server).
type Backend interface {
	Serve(ctx context.Context, c *domain.Cluster, kubeconfig []byte, readOnly bool, term Conn) error
}

// Conn is the minimal bidirectional terminal transport every backend shares, so identical session
// logic works whether the peer is a browser WebSocket (API side) or the API's proxy connection
// (worker side). Binary messages carry raw terminal bytes (stdin/stdout); text messages carry a
// JSON Control message (resize / exit / error).
type Conn interface {
	// ReadMessage returns the next message; isText marks a JSON Control frame vs. raw terminal bytes.
	ReadMessage() (isText bool, data []byte, err error)
	// WriteBinary sends raw terminal output bytes to the peer.
	WriteBinary(data []byte) error
	// WriteText sends a JSON Control message to the peer.
	WriteText(data []byte) error
	Close() error
}

// Control is a JSON terminal control message carried in a text frame. "resize" (client→server)
// carries Rows/Cols; "exit" (server→client) carries Code; "error" (server→client) carries Message.
type Control struct {
	Type    string `json:"type"`
	Rows    uint16 `json:"rows,omitempty"`
	Cols    uint16 `json:"cols,omitempty"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Handshake is the first text frame the API's proxy backend sends to the worker exec agent to open
// a session. Kubeconfig is JSON-encoded as base64 (Go's []byte encoding), so it rides the same
// text-frame channel as the control messages. Name/K8sVersion are cosmetic (prompt + hint), so the
// stateless agent needs no store access to build the session.
type Handshake struct {
	ClusterID  string `json:"cluster_id"`
	Name       string `json:"name"`
	K8sVersion string `json:"k8s_version"`
	Kubeconfig []byte `json:"kubeconfig"`
	Rows       uint16 `json:"rows"`
	Cols       uint16 `json:"cols"`
}

// Default terminal geometry until the client sends its first resize.
const (
	DefaultRows uint16 = 24
	DefaultCols uint16 = 80
)

// WriteControl marshals and sends a Control message as a text frame.
func WriteControl(term Conn, c Control) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return term.WriteText(b)
}

// WriteError reports a fatal session error to the peer (best-effort) before the caller closes.
func WriteError(term Conn, msg string) {
	_ = WriteControl(term, Control{Type: "error", Message: msg})
}

// Bridge copies messages both ways between two Conns until either side errors or closes, returning
// the first error. It is the entire body of every proxy hop in both terminal seams (internal/shell/
// proxy and internal/nodessh/proxy): once the handshake is sent, the API is a dumb byte pipe and the
// framing is identical in both directions, so neither hop needs to understand what it carries.
//
// Each side gets exactly one reader and one writer goroutine, which is what the WebSocket conns
// require - do not add a second reader on either Conn.
func Bridge(a, b Conn) error {
	errc := make(chan error, 2)
	go copyMessages(a, b, errc)
	go copyMessages(b, a, errc)
	return <-errc
}

func copyMessages(src, dst Conn, errc chan<- error) {
	for {
		isText, data, err := src.ReadMessage()
		if err != nil {
			errc <- err
			return
		}
		if isText {
			err = dst.WriteText(data)
		} else {
			err = dst.WriteBinary(data)
		}
		if err != nil {
			errc <- err
			return
		}
	}
}

// readLimit is the max WebSocket message we accept. Generous so a large burst of PTY output in one
// frame is never truncated; stdin/control frames are tiny.
const readLimit = 1 << 20

// NewConn adapts a coder/websocket connection to Conn, using ctx for every read/write. It is used
// by the API (wrapping the browser socket), the proxy (wrapping the worker socket), and the agent
// (wrapping its accepted socket) - every hop speaks the same framing.
func NewConn(ctx context.Context, ws *websocket.Conn) Conn {
	ws.SetReadLimit(readLimit)
	return &wsConn{ctx: ctx, ws: ws}
}

type wsConn struct {
	ctx context.Context
	ws  *websocket.Conn
}

func (c *wsConn) ReadMessage() (bool, []byte, error) {
	typ, data, err := c.ws.Read(c.ctx)
	return typ == websocket.MessageText, data, err
}

func (c *wsConn) WriteBinary(data []byte) error {
	return c.ws.Write(c.ctx, websocket.MessageBinary, data)
}

func (c *wsConn) WriteText(data []byte) error {
	return c.ws.Write(c.ctx, websocket.MessageText, data)
}

func (c *wsConn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}
