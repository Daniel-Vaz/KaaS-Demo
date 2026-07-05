package agent

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPProxyForwardsThroughServiceProxy checks the tunnel handler rewrites the request onto the
// API server's service-proxy path - stripping our /http-proxy prefix and preserving the app-facing
// route-prefix - and streams the upstream response back. The stub TLS server stands in for the
// cluster API server; the kubeconfig trusts it via certificate-authority-data.
func TestHTTPProxyForwardsThroughServiceProxy(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	var originPresent bool
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		_, originPresent = r.Header["Origin"]
		_, _ = io.WriteString(w, "hello from grafana")
	}))
	defer upstream.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	kc := []byte(`apiVersion: v1
clusters:
- name: c
  cluster:
    server: ` + upstream.URL + `
    certificate-authority-data: ` + base64.StdEncoding.EncodeToString(caPEM) + `
contexts:
- {name: ctx, context: {cluster: c, user: u}}
current-context: ctx
users:
- name: u
  user:
    token: cluster-admin-token
`)

	ag := New("", nil, nil, "", nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/http-proxy/api/clusters/c1/proxy/grafana/dashboards?tab=1", nil)
	req.Header.Set("X-Kaas-Kubeconfig", base64.StdEncoding.EncodeToString(kc))
	req.Header.Set("X-Kaas-Namespace", "monitoring-system")
	req.Header.Set("X-Kaas-Service", "kube-prometheus-stack-grafana")
	req.Header.Set("X-Kaas-Port", "80")
	req.Header.Set("Origin", "http://localhost:8080")
	req.Header.Set("Accept-Encoding", "gzip")

	ag.handleHTTPProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello from grafana" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	wantPath := "/api/v1/namespaces/monitoring-system/services/kube-prometheus-stack-grafana:80/proxy/api/clusters/c1/proxy/grafana/dashboards"
	if gotPath != wantPath {
		t.Fatalf("upstream path = %q, want %q", gotPath, wantPath)
	}
	if gotQuery != "tab=1" {
		t.Fatalf("upstream query = %q, want tab=1", gotQuery)
	}
	// The cluster's own bearer token must replace the agent token upstream.
	if gotAuth != "Bearer cluster-admin-token" {
		t.Fatalf("upstream auth = %q, want the cluster token", gotAuth)
	}
	// Origin is dropped: behind the tunnel the app's CSRF origin check compares it against the API
	// server's Host and could never pass (Grafana would 403 "origin not allowed" on login).
	if originPresent {
		t.Fatalf("Origin must be stripped before the upstream's CSRF origin check")
	}
}

// TestHTTPProxyUndoesAPIServerRewrite simulates the API server's service-proxy prepending its
// internal proxy path to the response's absolute URLs (<base href>, Location). The agent must strip
// that prefix back out so the browser resolves assets against the app's own reachable route-prefix.
func TestHTTPProxyUndoesAPIServerRewrite(t *testing.T) {
	const prefix = "/api/v1/namespaces/monitoring-system/services/kube-prometheus-stack-grafana:80/proxy"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == prefix+"/api/clusters/c1/proxy/grafana/" {
			// Emulate the API server rewrite: absolute URLs carry the internal proxy prefix.
			w.Header().Set("Location", prefix+"/api/clusters/c1/proxy/grafana/login")
			w.WriteHeader(http.StatusFound)
			return
		}
		// Served GZIPPED, like a real Grafana: the agent drops the browser's Accept-Encoding so Go's
		// transport negotiates gzip itself and transparently decompresses, keeping the body rewritable.
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, `<base href="`+prefix+`/api/clusters/c1/proxy/grafana/"/>`)
		_ = gz.Close()
	}))
	defer upstream.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	kc := []byte("apiVersion: v1\nclusters:\n- name: c\n  cluster:\n    server: " + upstream.URL +
		"\n    certificate-authority-data: " + base64.StdEncoding.EncodeToString(caPEM) +
		"\ncontexts:\n- {name: ctx, context: {cluster: c, user: u}}\ncurrent-context: ctx\nusers:\n- {name: u, user: {token: t}}\n")
	ag := New("", nil, nil, "", nil)

	newReq := func(p string) *http.Request {
		req := httptest.NewRequest("GET", "/http-proxy"+p, nil)
		req.Header.Set("X-Kaas-Kubeconfig", base64.StdEncoding.EncodeToString(kc))
		req.Header.Set("X-Kaas-Namespace", "monitoring-system")
		req.Header.Set("X-Kaas-Service", "kube-prometheus-stack-grafana")
		req.Header.Set("X-Kaas-Port", "80")
		return req
	}

	// HTML body: the injected prefix is stripped, leaving the app's reachable route-prefix.
	rec := httptest.NewRecorder()
	ag.handleHTTPProxy(rec, newReq("/api/clusters/c1/proxy/grafana/index"))
	if body := rec.Body.String(); body != `<base href="/api/clusters/c1/proxy/grafana/"/>` {
		t.Fatalf("body not rewritten: %q", body)
	}

	// Redirect Location: same treatment.
	rec = httptest.NewRecorder()
	ag.handleHTTPProxy(rec, newReq("/api/clusters/c1/proxy/grafana/"))
	if loc := rec.Header().Get("Location"); loc != "/api/clusters/c1/proxy/grafana/login" {
		t.Fatalf("Location not rewritten: %q", loc)
	}
}

// An app that cannot be told its base path (Longhorn) gets the opposite treatment: the API server's
// internal prefix is replaced by the BROWSER-facing one rather than stripped, and the document picks
// up the client-side shim that re-bases whatever its JavaScript builds at runtime.
func TestHTTPProxyRebasesAnAppWithNoRoutePrefix(t *testing.T) {
	const prefix = "/api/v1/namespaces/longhorn-system/services/longhorn-frontend:80/proxy"
	const route = "/api/clusters/c1/proxy/longhorn"
	var gotPaths []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, strings.TrimPrefix(r.URL.Path, prefix))
		w.Header().Set("Content-Type", "text/html")
		// A strict CSP would block the injected inline script; the app set it believing it sat at the
		// root, where it needed no shim.
		w.Header().Set("Content-Security-Policy", "script-src 'self'")
		// The API server rewrote the asset link it found in the document; the SPA's own `/v1/...`
		// calls live in the bundle and are untouched by anything server-side.
		_, _ = io.WriteString(w, `<html><head><script src="`+prefix+`/index.js"></script></head><body></body></html>`)
	}))
	defer upstream.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	kc := []byte("apiVersion: v1\nclusters:\n- name: c\n  cluster:\n    server: " + upstream.URL +
		"\n    certificate-authority-data: " + base64.StdEncoding.EncodeToString(caPEM) +
		"\ncontexts:\n- {name: ctx, context: {cluster: c, user: u}}\ncurrent-context: ctx\nusers:\n- {name: u, user: {token: t}}\n")
	ag := New("", nil, nil, "", nil)

	req := httptest.NewRequest("GET", "/http-proxy"+route+"/", nil)
	req.Header.Set("X-Kaas-Kubeconfig", base64.StdEncoding.EncodeToString(kc))
	req.Header.Set("X-Kaas-Namespace", "longhorn-system")
	req.Header.Set("X-Kaas-Service", "longhorn-frontend")
	req.Header.Set("X-Kaas-Port", "80")
	req.Header.Set("X-Kaas-Route-Prefix", route)
	rec := httptest.NewRecorder()
	ag.handleHTTPProxy(rec, req)

	body := rec.Body.String()
	// The asset link is re-based onto the tunnel path, not stripped to "/index.js" - which would
	// resolve against the portal's own origin and 404.
	if !strings.Contains(body, `src="`+route+`/index.js"`) {
		t.Errorf("asset link not re-based: %s", body)
	}
	if !strings.Contains(body, `var p="`+route+`"`) {
		t.Errorf("no base-path shim injected: %s", body)
	}
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Error("the CSP must be dropped, or the browser blocks the injected shim")
	}
	// THE other half: the app itself must be addressed at its own root. Longhorn's nginx serves "/"
	// and "/v1" only - handing it the tunnel path would fall through to its SPA catch-all and return
	// index.html for every asset.
	if len(gotPaths) != 1 || gotPaths[0] != "/" {
		t.Fatalf("upstream saw %v, want the tunnel path stripped to /", gotPaths)
	}

	// An asset request under the tunnel path reaches the app as a root-relative one.
	req = httptest.NewRequest("GET", "/http-proxy"+route+"/index.js", nil)
	req.Header.Set("X-Kaas-Kubeconfig", base64.StdEncoding.EncodeToString(kc))
	req.Header.Set("X-Kaas-Namespace", "longhorn-system")
	req.Header.Set("X-Kaas-Service", "longhorn-frontend")
	req.Header.Set("X-Kaas-Port", "80")
	req.Header.Set("X-Kaas-Route-Prefix", route)
	ag.handleHTTPProxy(httptest.NewRecorder(), req)
	if len(gotPaths) != 2 || gotPaths[1] != "/index.js" {
		t.Fatalf("upstream saw %v, want /index.js", gotPaths)
	}
}
