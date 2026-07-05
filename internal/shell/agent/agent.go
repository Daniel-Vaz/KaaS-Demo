// Package agent is the exec agent: a small HTTP/WebSocket server the API dials to reach a cluster. It
// runs on a host-networked container (the only place with a network route to the cluster API server;
// see docs/networking.md) - in real mode the dedicated, unprivileged `shell` sandbox
// (cmd/shell-agent), and in the single-process dev path (make run-worker) the worker itself. It
// serves two kinds of request, both authenticated by a shared bearer token:
//
//   - the interactive shell (GET /exec): a real bash+kubectl PTY (internal/shell/pty), and
//   - the Workloads seam (POST /kube-exec, GET /kube-logs): one-shot kubectl invocations and
//     streaming `kubectl logs -f`, run via a kubectl.LocalExecer (internal/kube/kubectl).
//
// Every request carries its own cluster kubeconfig, so the agent is stateless. Auth is a shared
// bearer token (KAAS_SHELL_TOKEN) so only the API - not other host processes - can reach it. It is a
// plaintext localhost token, not TLS/mTLS; see the security note in internal/shell.
package agent

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube/kubectl"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kubeconfig"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell/pty"
	"github.com/coder/websocket"
)

// Agent serves shell PTY sessions and kubectl invocations over HTTP/WebSocket.
type Agent struct {
	token        string
	runner       *pty.Runner
	kube         *kubectl.LocalExecer
	kubeProxyURL string // SOCKS proxy for a remote KVM host; "" = dial cluster API servers directly
	log          *slog.Logger
}

// New builds an agent. An empty token disables auth (dev/local only) - the caller should warn. The
// kubeExec runs the Workloads seam's kubectl commands; it may be nil to serve only the shell.
// kubeProxyURL routes the tunnel seam's HTTP proxy (/http-proxy) through the remote-KVM SOCKS tunnel
// when set, mirroring how the PTY and kubectl execer are pointed at it; "" dials API servers directly.
func New(token string, runner *pty.Runner, kubeExec *kubectl.LocalExecer, kubeProxyURL string, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	return &Agent{token: token, runner: runner, kube: kubeExec, kubeProxyURL: kubeProxyURL, log: log}
}

// Serve runs the agent HTTP server on addr and blocks until ctx is cancelled (or it fails to
// listen). Non-fatal: the caller runs it in its own goroutine alongside the reconciler.
func (a *Agent) Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /exec", a.handleExec)
	mux.HandleFunc("POST /kube-exec", a.handleKubeExec)
	mux.HandleFunc("GET /kube-logs", a.handleKubeLogs)
	mux.HandleFunc("/http-proxy/", a.handleHTTPProxy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	a.log.Info("shell exec agent listening", "addr", addr, "auth", a.token != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *Agent) handleExec(w http.ResponseWriter, r *http.Request) {
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
	var hs shell.Handshake
	if err := json.Unmarshal(data, &hs); err != nil {
		shell.WriteError(term, "bad handshake")
		_ = conn.Close(websocket.StatusProtocolError, "bad handshake")
		return
	}

	// The runner only needs these fields (ID for the temp dir, Name/K8sVersion for the prompt/hint);
	// the kubeconfig is what makes kubectl work.
	c := &domain.Cluster{ID: hs.ClusterID, Name: hs.Name, K8sVersion: hs.K8sVersion}
	a.log.Info("shell session started", "cluster", c.Name)
	if err := a.runner.Serve(ctx, c, hs.Kubeconfig, hs.Rows, hs.Cols, term); err != nil {
		a.log.Warn("shell session error", "cluster", c.Name, "err", err)
		shell.WriteError(term, err.Error())
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	a.log.Info("shell session ended", "cluster", c.Name)
}

// handleKubeExec runs one kubectl command for a cluster and returns its captured output. Stateless:
// the kubeconfig rides in the request, written to a private temp file by the LocalExecer.
func (a *Agent) handleKubeExec(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if a.kube == nil {
		http.Error(w, "kube exec not configured", http.StatusServiceUnavailable)
		return
	}
	var req kube.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	res, err := a.kube.RunInput(r.Context(), req.Kubeconfig, req.ClusterID, req.Stdin, req.Args)
	resp := kube.ExecResponse{Stdout: res.Stdout, Stderr: res.Stderr, Code: res.Code}
	if err != nil {
		resp.Error = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleHTTPProxy is the tunnel seam's real backend (internal/tunnel): a full streaming HTTP reverse
// proxy from the API to an in-cluster web UI (Grafana/Prometheus/Alertmanager), routed - like every
// other cluster access - through the API server's `services/<svc>:<port>/proxy` endpoint, the only
// path this host-networked agent has to the cluster (docs/networking.md).
//
// It is stateless like the rest of the agent: the request carries its own admin kubeconfig (base64
// header) and target service (headers), from which a per-request transport is built. The incoming
// path is "/http-proxy" + the browser-facing app path (e.g. /http-proxy/api/clusters/<id>/proxy/
// grafana/…); we strip our prefix and hand the remainder to the service proxy, so the app - which
// was configured at install with a matching route-prefix - sees the path it expects and its
// absolute-path assets resolve without any response rewriting.
func (a *Agent) handleHTTPProxy(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	kcB64 := r.Header.Get("X-Kaas-Kubeconfig")
	kc, err := base64.StdEncoding.DecodeString(kcB64)
	if err != nil || len(kc) == 0 {
		http.Error(w, "missing or invalid kubeconfig", http.StatusBadRequest)
		return
	}
	ns := r.Header.Get("X-Kaas-Namespace")
	svc := r.Header.Get("X-Kaas-Service")
	port := r.Header.Get("X-Kaas-Port")
	if ns == "" || svc == "" || port == "" {
		http.Error(w, "missing target service", http.StatusBadRequest)
		return
	}

	ep, err := kubeconfig.NewEndpoint(kc, a.kubeProxyURL)
	if err != nil {
		a.log.Warn("http-proxy endpoint", "err", err)
		http.Error(w, "cannot build cluster endpoint: "+err.Error(), http.StatusBadGateway)
		return
	}
	server, err := url.Parse(ep.Server)
	if err != nil {
		http.Error(w, "bad cluster server url", http.StatusBadGateway)
		return
	}
	// The service-proxy prefix; the app-facing path (with our /http-proxy stripped) is appended so
	// the app receives its full configured route-prefix.
	proxyPrefix := "/api/v1/namespaces/" + ns + "/services/" + svc + ":" + port + "/proxy"
	appPath := strings.TrimPrefix(r.URL.Path, "/http-proxy")
	// What the browser should see where the API server put its own proxy path - empty for an app that
	// serves itself under the tunnel path already. See ModifyResponse.
	routePrefix := r.Header.Get("X-Kaas-Route-Prefix")
	// ...and, for that same app, the tunnel path has to come OFF the request on the way in. A
	// self-prefixed app (Grafana et al.) was configured to serve under it and expects to see it; an
	// app that knows nothing about it (Longhorn) serves "/" and "/v1" only, so forwarding
	// "/api/clusters/<id>/proxy/longhorn/index.js" would miss every route it has and fall through to
	// its SPA catch-all - index.html returned for a script request, and a blank page.
	//
	// The two directions are exact inverses: strip the prefix here, put it back in the response (and
	// in the shim, for the URLs only the browser ever sees).
	if routePrefix != "" {
		appPath = strings.TrimPrefix(appPath, routePrefix)
		if appPath == "" {
			appPath = "/"
		}
	}

	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // stream at once - matters for Grafana Live / long responses
		Transport:     ep.Transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, e error) {
			a.log.Warn("http-proxy upstream", "service", svc, "err", e)
			http.Error(w, "cluster UI unreachable: "+e.Error(), http.StatusBadGateway)
		},
		Director: func(req *http.Request) {
			req.URL.Scheme = server.Scheme
			req.URL.Host = server.Host
			req.URL.Path = proxyPrefix + appPath
			req.URL.RawPath = ""
			req.Host = server.Host
			// Force an uncompressed response so ModifyResponse can rewrite the body (below).
			req.Header.Del("Accept-Encoding")
			// Grafana (and friends) run a CSRF check that compares the browser's Origin against the Host
			// they see. Behind this tunnel that Host is the API SERVER's, which no legitimate browser
			// Origin can ever match - so the check rejects every state-changing request ("origin not
			// allowed" on login) and can never be satisfied. Drop the header so the app skips the check:
			// the tunnel's real CSRF boundary is the API, which requires our SameSite=Lax session cookie,
			// so a cross-site POST is never authenticated here in the first place. Production, sitting
			// behind a fixed portal hostname, would set Grafana's csrf_trusted_origins to it instead.
			req.Header.Del("Origin")
			// Replace the agent's bearer token with the cluster's own auth (or none, for client-cert
			// kubeconfigs) so the shell token never reaches the API server, and drop our internal headers.
			if ep.Token != "" {
				req.Header.Set("Authorization", "Bearer "+ep.Token)
			} else {
				req.Header.Del("Authorization")
			}
			req.Header.Del("X-Kaas-Kubeconfig")
			req.Header.Del("X-Kaas-Namespace")
			req.Header.Del("X-Kaas-Service")
			req.Header.Del("X-Kaas-Port")
			req.Header.Del("X-Kaas-Route-Prefix")
		},
		// The API server's own service-proxy rewrites absolute-path URLs in the response - <base href>,
		// asset links, redirect Location - by PREPENDING its internal proxy path (proxyPrefix). Left as
		// is, the browser would resolve every asset against that internal path (which routes to no API
		// endpoint) and 404. The rewrite is deterministic, so we undo it - replacing proxyPrefix with
		// whatever the browser SHOULD see in its place:
		//
		//   - nothing, for an app configured at install to serve itself under the tunnel path
		//     (tunnel.RoutePrefix): its URLs already carry that prefix underneath the API server's,
		//     so stripping restores exactly what the browser can reach. Grafana, Prometheus,
		//     Alertmanager.
		//   - the tunnel path itself, for an app with no such setting (Longhorn), whose URLs are rooted
		//     at "/" and would otherwise land on the portal's own origin.
		//
		// The caller decides which by sending X-Kaas-Route-Prefix, and only the caller can: the agent
		// is stateless and knows nothing about clusters or apps.
		ModifyResponse: func(resp *http.Response) error {
			if loc := resp.Header.Get("Location"); strings.Contains(loc, proxyPrefix) {
				resp.Header.Set("Location", strings.ReplaceAll(loc, proxyPrefix, routePrefix))
			}
			if !rewritableBody(resp.Header.Get("Content-Type")) {
				return nil // binary assets (js/img/fonts) carry no rewritten absolute paths
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return err
			}
			body = bytes.ReplaceAll(body, []byte(proxyPrefix), []byte(routePrefix))
			// Rewriting the DOCUMENT is only half the job for an app that cannot be told its base
			// path: the URLs its JavaScript builds at RUNTIME never pass through here at all. Inject
			// a shim that re-bases them in the browser instead - see basePathShim.
			if routePrefix != "" && isHTML(resp.Header.Get("Content-Type")) {
				body = injectBasePathShim(body, routePrefix)
				// The shim is an inline <script>, which a Content-Security-Policy without
				// 'unsafe-inline' would block - and the app served it believing it sat at the root,
				// where it needed no shim. Dropping the header costs nothing here: the tunnel is the
				// only route to this app, every request is already gated by the API, and the page is
				// same-origin with the portal that framed it.
				resp.Header.Del("Content-Security-Policy")
				resp.Header.Del("Content-Security-Policy-Report-Only")
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			resp.Header.Del("Content-Encoding")
			return nil
		},
	}
	rp.ServeHTTP(w, r)
}

// rewritableBody reports whether a response body may carry the absolute-path URLs the API server's
// service-proxy rewrites (the HTML document's <base href>/links and CSS url()s). JS bundles and
// binary assets are served through untouched, so we skip the cost of scanning them.
func rewritableBody(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "text/css")
}

// handleKubeLogs streams `kubectl logs [-f]` for a pod over a WebSocket: a one-time text handshake
// (LogsRequest), then raw log bytes as binary frames until the command exits or the peer leaves.
func (a *Agent) handleKubeLogs(w http.ResponseWriter, r *http.Request) {
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

	if a.kube == nil {
		shell.WriteError(term, "kube exec not configured")
		_ = conn.Close(websocket.StatusPolicyViolation, "no kube")
		return
	}
	isText, data, err := term.ReadMessage()
	if err != nil || !isText {
		shell.WriteError(term, "expected logs handshake")
		_ = conn.Close(websocket.StatusProtocolError, "no handshake")
		return
	}
	var req kube.LogsRequest
	if err := json.Unmarshal(data, &req); err != nil {
		shell.WriteError(term, "bad logs handshake")
		_ = conn.Close(websocket.StatusProtocolError, "bad handshake")
		return
	}
	if err := a.kube.Stream(ctx, req.Kubeconfig, req.ClusterID, req.Args, &connSink{conn: conn, ctx: ctx}); err != nil {
		shell.WriteError(term, err.Error())
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// connSink adapts a WebSocket to kube.LogSink, forwarding log bytes as binary frames.
type connSink struct {
	conn *websocket.Conn
	ctx  context.Context
}

func (s *connSink) Write(p []byte) error {
	return s.conn.Write(s.ctx, websocket.MessageBinary, p)
}

func (a *Agent) authorized(r *http.Request) bool {
	if a.token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
}
