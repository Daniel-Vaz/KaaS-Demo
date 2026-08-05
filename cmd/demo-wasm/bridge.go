//go:build js && wasm

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall/js"
)

// The JS bridge: everything the page's shim (web/portal/src/demo/shim.ts) is allowed to call.
//
// There are exactly two entry points, because there are exactly two shapes of traffic the portal
// generates: request/response-or-stream (fetch, and the SSE event feed, which is only a fetch whose
// body never ends) and the bidirectional terminals (the cluster shell, node SSH, live pod logs).
//
// Neither ever blocks the JS event loop: each call starts a goroutine and returns immediately, and
// results arrive on the callbacks the shim supplied. Go's wasm scheduler runs those goroutines on
// the same single JS thread, so touching js.Value from them is safe.

// exportBridge installs the bridge on globalThis as `__kaas`.
func exportBridge(h http.Handler, srv sessionRunner) {
	js.Global().Set("__kaas", js.ValueOf(map[string]any{
		"fetch":    js.FuncOf(func(_ js.Value, args []js.Value) any { return jsFetch(h, args[0], args[1]) }),
		"terminal": js.FuncOf(func(_ js.Value, args []js.Value) any { return jsTerminal(srv, args[0], args[1]) }),
	}))
}

// jsFetch drives one HTTP request through the API handler.
//
// req: {method, url, headers: {name: value}, body: string}
// cbs: {onHead(status, headersObj), onChunk(Uint8Array), onEnd(errString|null)}
//
// It returns an abort function. Aborting is not a nicety: the SSE handler blocks on the request
// context until the client goes away, so without it every closed event stream would leak a
// goroutine and a broker subscription for the life of the page.
func jsFetch(h http.Handler, req, cbs js.Value) any {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		defer cancel()
		r, err := buildRequest(ctx, req)
		if err != nil {
			call(cbs, "onEnd", err.Error())
			return
		}
		w := &streamWriter{hdr: make(http.Header), cbs: cbs}
		h.ServeHTTP(w, r)
		w.finish()
		call(cbs, "onEnd", js.Null())
	}()

	return js.FuncOf(func(js.Value, []js.Value) any { cancel(); return nil })
}

// buildRequest turns the shim's plain-object request into an *http.Request for the handler.
func buildRequest(ctx context.Context, req js.Value) (*http.Request, error) {
	method := req.Get("method").String()
	url := req.Get("url").String()
	var body io.Reader
	if b := req.Get("body"); b.Type() == js.TypeString {
		body = strings.NewReader(b.String())
	}
	r, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if hdrs := req.Get("headers"); hdrs.Type() == js.TypeObject {
		keys := js.Global().Get("Object").Call("keys", hdrs)
		for i := range keys.Length() {
			k := keys.Index(i).String()
			r.Header.Set(k, hdrs.Get(k).String())
		}
	}
	// The login throttle keys on this (internal/app/throttle.go). Every visitor is their own
	// isolated instance, so there is only ever one client - but the field must be well-formed.
	r.RemoteAddr = "127.0.0.1:0"
	return r, nil
}

// streamWriter is the http.ResponseWriter the handler writes into: each Write is pushed straight to
// JS as a chunk, which is what makes the SSE feed live rather than buffered. It implements
// http.Flusher because the event-stream handler type-asserts for it and 500s without it.
type streamWriter struct {
	hdr    http.Header
	cbs    js.Value
	status int
	sent   bool
}

func (w *streamWriter) Header() http.Header { return w.hdr }

func (w *streamWriter) WriteHeader(status int) {
	if w.sent {
		return
	}
	w.status = status
	w.sent = true
	hdrs := make(map[string]any, len(w.hdr))
	for k := range w.hdr {
		hdrs[k] = w.hdr.Get(k)
	}
	call(w.cbs, "onHead", status, js.ValueOf(hdrs))
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if !w.sent {
		w.WriteHeader(http.StatusOK)
	}
	call(w.cbs, "onChunk", toUint8Array(p))
	return len(p), nil
}

// Flush is a no-op: Write already delivered the bytes.
func (w *streamWriter) Flush() {}

// finish covers a handler that returned without writing anything (a 200 with an empty body).
func (w *streamWriter) finish() {
	if !w.sent {
		w.WriteHeader(http.StatusOK)
	}
}

// sessionRunner is the slice of *api.Server the terminal bridge needs. An interface rather than the
// concrete type so this file states its dependency exactly, and so a test can drive it.
type sessionRunner interface {
	openTerminal(ctx context.Context, r *http.Request, term *jsConn) (int, string)
}

// jsTerminal opens one terminal session (cluster shell, node SSH or pod logs - the route decides).
//
// req: {url, headers} - the same shape jsFetch takes, since a terminal is identified by exactly the
// URL and cookie the real WebSocket handshake would have carried.
// cbs: {onOpen(), onBinary(Uint8Array), onText(string), onClose(code, reason)}
//
// It returns {send(Uint8Array), sendText(string), close()} - the WebSocket surface the portal's
// TerminalSession and LogViewer already speak, so neither component needs to know it isn't one.
func jsTerminal(srv sessionRunner, req, cbs js.Value) any {
	ctx, cancel := context.WithCancel(context.Background())
	term := &jsConn{ctx: ctx, cancel: cancel, in: make(chan jsMsg, 32), cbs: cbs}

	send := js.FuncOf(func(_ js.Value, args []js.Value) any { term.push(false, fromUint8Array(args[0])); return nil })
	sendText := js.FuncOf(func(_ js.Value, args []js.Value) any { term.push(true, []byte(args[0].String())); return nil })
	closeFn := js.FuncOf(func(js.Value, []js.Value) any { cancel(); return nil })

	go func() {
		defer func() {
			cancel()
			// Release the callbacks, or every closed terminal pins its closures for the life of the page.
			send.Release()
			sendText.Release()
			closeFn.Release()
		}()
		r, err := buildRequest(ctx, req)
		if err != nil {
			call(cbs, "onClose", 1011, err.Error())
			return
		}
		call(cbs, "onOpen")
		code, reason := srv.openTerminal(ctx, r, term)
		term.closeOnce(code, reason)
	}()

	return js.ValueOf(map[string]any{"send": send, "sendText": sendText, "close": closeFn})
}

// jsConn is a shell.Conn whose peer is the browser rather than a WebSocket: reads come off a
// channel the JS `send` callbacks feed, writes go straight out as callbacks. The frame split is the
// same one the wire protocol uses - binary is terminal bytes, text is a JSON shell.Control message -
// so the session code above it cannot tell the difference.
type jsConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	in     chan jsMsg
	cbs    js.Value
	once   sync.Once
}

type jsMsg struct {
	isText bool
	data   []byte
}

// push hands a message from the browser to the session. It drops rather than blocks when the
// session is not reading: a terminal that has stopped consuming keystrokes is one that is closing,
// and blocking here would block the JS event loop's caller.
func (c *jsConn) push(isText bool, data []byte) {
	select {
	case c.in <- jsMsg{isText: isText, data: data}:
	case <-c.ctx.Done():
	default:
	}
}

func (c *jsConn) ReadMessage() (bool, []byte, error) {
	select {
	case <-c.ctx.Done():
		return false, nil, io.EOF
	case m := <-c.in:
		return m.isText, m.data, nil
	}
}

func (c *jsConn) WriteBinary(p []byte) error {
	if c.ctx.Err() != nil {
		return io.ErrClosedPipe
	}
	call(c.cbs, "onBinary", toUint8Array(p))
	return nil
}

func (c *jsConn) WriteText(p []byte) error {
	if c.ctx.Err() != nil {
		return io.ErrClosedPipe
	}
	call(c.cbs, "onText", string(p))
	return nil
}

func (c *jsConn) Close() error {
	c.closeOnce(1000, "")
	return nil
}

func (c *jsConn) closeOnce(code int, reason string) {
	c.once.Do(func() {
		c.cancel()
		call(c.cbs, "onClose", code, reason)
	})
}

// call invokes an optional callback on the shim's callback object, tolerating a missing one.
func call(cbs js.Value, name string, args ...any) {
	fn := cbs.Get(name)
	if fn.Type() != js.TypeFunction {
		return
	}
	fn.Invoke(args...)
}

func toUint8Array(p []byte) js.Value {
	buf := js.Global().Get("Uint8Array").New(len(p))
	js.CopyBytesToJS(buf, p)
	return buf
}

func fromUint8Array(v js.Value) []byte {
	p := make([]byte, v.Get("length").Int())
	js.CopyBytesToGo(p, v)
	return p
}
