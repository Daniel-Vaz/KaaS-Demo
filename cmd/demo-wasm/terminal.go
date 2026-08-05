//go:build js && wasm

package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Daniel-Vaz/KaaS-demo/internal/api"
	"github.com/Daniel-Vaz/KaaS-demo/internal/app"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
)

// terminals routes a bridged terminal session to the right API session handler.
//
// It exists because the three terminal endpoints are the one part of the portal that cannot go
// through the HTTP handler in this build: they are WebSocket upgrades, and there is no connection to
// hijack when the "server" is a function call. So the shim hands us the URL and cookie the handshake
// would have carried, and this re-does the two things the real handlers do before upgrading -
// resolve the actor, authorize the cluster - and then calls the very same session code
// (internal/api/session.go). Nothing about a session's behaviour is re-implemented here.
type terminals struct {
	srv *api.Server
	app *app.App
}

// openTerminal resolves r to a session and runs it on term until it ends, returning the WebSocket
// close code and reason the shim reports to the portal. Failures before the session starts are
// reported in-terminal, as they are in production once the upgrade has happened.
func (t *terminals) openTerminal(ctx context.Context, r *http.Request, term *jsConn) (int, string) {
	actor := t.srv.ResolveActor(r)
	if actor == nil {
		shell.WriteError(term, "your session has expired - reload the page and sign in again")
		return closePolicy, "unauthenticated"
	}

	// A ServeMux purely for its path-value matching, so the routes read exactly like the real ones
	// in internal/api. The handlers close over the result rather than writing a response.
	code, reason := closeError, "unknown terminal route"
	mux := http.NewServeMux()

	mux.HandleFunc("GET /clusters/{id}/shell", func(_ http.ResponseWriter, r *http.Request) {
		c, err := t.app.GetCluster(actor, r.PathValue("id")) // owner-or-admin-or-group-mate
		if err != nil {
			shell.WriteError(term, err.Error())
			code, reason = closePolicy, "forbidden"
			return
		}
		st, rs := t.srv.RunShellSession(ctx, actor, c, term)
		code, reason = int(st), rs
	})

	mux.HandleFunc("GET /clusters/{id}/nodes/{vm}/ssh", func(_ http.ResponseWriter, r *http.Request) {
		c, n, err := t.app.NodeSSHTarget(actor, r.PathValue("id"), r.PathValue("vm")) // write-scoped
		if err != nil {
			shell.WriteError(term, err.Error())
			code, reason = closePolicy, "forbidden"
			return
		}
		st, rs := t.srv.RunNodeSSHSession(ctx, actor, c, n, term)
		code, reason = int(st), rs
	})

	mux.HandleFunc("GET /clusters/{id}/workloads/{kind}/{namespace}/{name}/logs", func(_ http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tail, _ := strconv.Atoi(q.Get("tail"))
		ref := kube.LogRef{
			Namespace: r.PathValue("namespace"),
			Pod:       q.Get("pod"),
			Container: q.Get("container"),
			TailLines: tail,
			Follow:    q.Get("follow") == "1" || q.Get("follow") == "true",
		}
		st, rs := t.srv.RunLogSession(ctx, actor, r.PathValue("id"), ref, term)
		code, reason = int(st), rs
	})

	mux.ServeHTTP(discardWriter{}, r)
	return code, reason
}

// WebSocket close codes used when the session never started. The successful cases come back from
// the session functions themselves.
const (
	closePolicy = 1008 // policy violation
	closeError  = 1011 // internal error
)

// discardWriter satisfies the ServeMux's signature; the routes above never write a response, having
// a terminal to report into instead. A 404 from the mux leaves the initial code/reason in place.
type discardWriter struct{}

func (discardWriter) Header() http.Header         { return http.Header{} }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) WriteHeader(int)             {}
