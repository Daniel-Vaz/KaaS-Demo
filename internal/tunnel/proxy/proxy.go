// Package proxy is the API-side tunnel.Proxier for real (worker) mode. The API can't reach the
// cluster API server itself, so it reverse-proxies each browser request to the host-networked exec
// agent's /http-proxy endpoint (the same agent the shell and Workloads/Monitoring/Security seams
// use), which in turn proxies to the in-cluster UI via the API server's service proxy. Two chained
// reverse proxies, one per network hop - the only shape the networking constraint allows.
//
// It reuses the exec-agent pool and shared bearer token. Each request is self-contained (it carries
// its own admin kubeconfig and target service in headers), so any agent can serve any request: the
// Director round-robins the pool, keeping the tunnel horizontally scalable like the other seams.
package proxy

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/execagent"
	"github.com/Daniel-Vaz/KaaS-demo/internal/tunnel"
)

// grafanaRotatePath is the Grafana SPA's session-token rotation endpoint (see serveGrafanaRotateStub),
// matched at the tail of the browser-facing app path. sessionKeepaliveWindow is how far ahead the
// synthetic response pushes Grafana's grafana_session_expiry cookie so the SPA reschedules its next
// rotation comfortably in the future rather than immediately.
const (
	grafanaRotatePath      = "/api/user/auth-tokens/rotate"
	sessionKeepaliveWindow = time.Hour
)

// Proxier forwards tunnel requests to an exec agent.
type Proxier struct {
	pool  *execagent.Pool
	token string
	rp    *httputil.ReverseProxy
	log   *slog.Logger
}

// ctxKey carries the per-request tunnel parameters from Serve to the shared ReverseProxy Director.
type ctxKey struct{}

type reqParams struct {
	app     tunnel.App
	kc      []byte
	id      tunnel.Identity
	appPath string // the browser-facing app path, /api-prefixed (what the agent must forward on)
	// clusterID is needed only to rebuild the app's browser-facing route prefix for an app that
	// cannot be configured with one at install (tunnel.App.SelfPrefixed).
	clusterID string
}

// New builds the proxy Proxier over the given exec-agent pool and shared bearer token.
func New(pool *execagent.Pool, token string, log *slog.Logger) *Proxier {
	if log == nil {
		log = slog.Default()
	}
	p := &Proxier{pool: pool, token: token, log: log}
	p.rp = &httputil.ReverseProxy{
		FlushInterval: -1, // stream at once (SSE / Grafana Live / large assets)
		Director:      p.direct,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, e error) {
			p.log.Warn("tunnel proxy", "err", e)
			http.Error(w, "cluster UI unreachable", http.StatusBadGateway)
		},
	}
	return p
}

// Serve reverse-proxies one request to the cluster's app UI via a pool agent. Params ride the
// request context so the single shared ReverseProxy can read them in its Director.
func (p *Proxier) Serve(w http.ResponseWriter, r *http.Request, c *domain.Cluster, app tunnel.App, kc []byte, id tunnel.Identity) {
	// Grafana 13's SPA unconditionally rotates its session token (POST /api/user/auth-tokens/rotate)
	// whenever a grafana_session_expiry cookie is present. Auth-proxy sessions - how we sign users in,
	// keeping the credential-free per-user Editor/Viewer model (see internal/tunnel) - carry NO
	// server-side token, so the real endpoint 401s, and on 401 the SPA calls
	// setLoggedOut()→window.location.reload(): an infinite reload loop for any browser still holding a
	// stale expiry cookie. Grafana's own auth.proxy enable_login_token does not mint a token in v13, so
	// we answer the rotate ourselves - a 200 that pushes the expiry cookie forward - which makes the SPA
	// reschedule far ahead and stop spinning while auth-proxy keeps authenticating every real request.
	// Production, fronting Grafana with a fixed hostname, would run a real session client instead of this shim.
	if app.AuthProxy && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, grafanaRotatePath) {
		serveGrafanaRotateStub(w, r, tunnel.RoutePrefix(c.ID, app.ID))
		return
	}
	params := &reqParams{
		app:       app,
		kc:        kc,
		id:        id,
		clusterID: c.ID,
		// The API sees the path with nginx's /api already stripped; re-add PublicPrefix so the agent
		// forwards the full browser-facing path the app was configured to serve under.
		appPath: tunnel.PublicPrefix + r.URL.Path,
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, params)
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// serveGrafanaRotateStub answers Grafana's session-token rotation with a synthetic success that pushes
// the grafana_session_expiry cookie sessionKeepaliveWindow into the future. The SPA reads that cookie
// (via document.cookie, so it must NOT be HttpOnly - mirroring Grafana's own cookie, which isn't either)
// to schedule its next rotation, so a future value stops the immediate re-rotation; a 200 (vs. the real
// 401) stops the reload loop. cookiePath is the app's browser-facing route prefix (tunnel.RoutePrefix),
// matching the path Grafana itself scopes the cookie to, so the browser overwrites the stale one rather
// than keeping a second copy. Secure follows the inbound request's own scheme, same as the portal's own
// session cookie (internal/api.requestIsHTTPS) - plain HTTP in the local demo, behind a TLS-terminating
// edge in production.
func serveGrafanaRotateStub(w http.ResponseWriter, r *http.Request, cookiePath string) {
	// codeql[go/cookie-httponly-not-set] -- deliberately not HttpOnly: the value is a UNIX timestamp the
	// Grafana SPA itself reads via document.cookie to schedule its next rotation, never a credential,
	// and this stub only mirrors the (also non-HttpOnly) cookie Grafana's own frontend sets.
	//
	// codeql[go/cookie-secure-not-set] -- Secure is conditional on the inbound request's own scheme
	// (requestIsHTTPS), not omitted or hardcoded false - see internal/api.setSessionCookie for why a
	// hardcoded true would break the plain-HTTP local demo.
	http.SetCookie(w, &http.Cookie{
		Name:     "grafana_session_expiry",
		Value:    strconv.FormatInt(time.Now().Add(sessionKeepaliveWindow).Unix(), 10),
		Path:     cookiePath,
		MaxAge:   int(sessionKeepaliveWindow / time.Second),
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"message":"Token rotated"}`))
}

// requestIsHTTPS reports whether the request reached the API over TLS - directly, or via a proxy that
// terminated it and said so. Same check as internal/api.requestIsHTTPS; duplicated rather than shared
// to avoid a cross-package dependency for three lines.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (p *Proxier) direct(req *http.Request) {
	params, _ := req.Context().Value(ctxKey{}).(*reqParams)
	if params == nil {
		return
	}
	addr := p.pool.Candidates()[0] // any agent can serve it; the first is the round-robin choice
	req.URL.Scheme = "http"
	req.URL.Host = addr
	req.URL.Path = "/http-proxy" + params.appPath
	req.URL.RawPath = ""
	req.Host = addr
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	req.Header.Set("X-Kaas-Kubeconfig", base64.StdEncoding.EncodeToString(params.kc))
	req.Header.Set("X-Kaas-Namespace", params.app.Namespace)
	req.Header.Set("X-Kaas-Service", params.app.Service)
	req.Header.Set("X-Kaas-Port", params.app.Port)
	// What the agent should put where the API server's service-proxy prefixed the app's absolute URLs.
	// Empty for an app already configured to serve itself under its tunnel path; the tunnel path itself
	// for one that cannot be (Longhorn). See tunnel.App.SelfPrefixed.
	if params.app.SelfPrefixed {
		req.Header.Del("X-Kaas-Route-Prefix")
	} else {
		req.Header.Set("X-Kaas-Route-Prefix", tunnel.RoutePrefix(params.clusterID, params.app.ID))
	}

	// The auth-proxy identity an AuthProxy app (Grafana) is signed in as. Delete first, ALWAYS: the
	// browser may have sent its own X-Webauth-* headers, and forwarding those would let any user
	// self-assign a role. Only the server-resolved Identity is ever set, and only for an app that
	// consumes it.
	req.Header.Del(tunnel.HeaderProxyUser)
	req.Header.Del(tunnel.HeaderProxyRole)
	if params.app.AuthProxy {
		req.Header.Set(tunnel.HeaderProxyUser, params.id.User)
		req.Header.Set(tunnel.HeaderProxyRole, params.id.Role)
	}
}
