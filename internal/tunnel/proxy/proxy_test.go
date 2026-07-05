package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/execagent"
	"github.com/Daniel-Vaz/KaaS-demo/internal/tunnel"
)

// serveVia runs one request through the Proxier against a stub exec agent and returns the headers
// the agent saw.
func serveVia(t *testing.T, app tunnel.App, id tunnel.Identity, clientHdr map[string]string) http.Header {
	t.Helper()
	var got http.Header
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	defer agent.Close()

	p := New(execagent.NewPool(strings.TrimPrefix(agent.URL, "http://")), "agent-token", nil)
	req := httptest.NewRequest("GET", "/clusters/c1/proxy/"+app.ID+"/", nil)
	for k, v := range clientHdr {
		req.Header.Set(k, v)
	}
	p.Serve(httptest.NewRecorder(), req, &domain.Cluster{ID: "c1"}, app, []byte("kubeconfig"), id)
	return got
}

// TestIdentityHeadersAreAuthoritative is the security property behind the tunnel's role mapping:
// Grafana trusts the auth-proxy headers, so a browser-supplied copy must NEVER survive. A user who
// sends X-Webauth-Role: Admin must still arrive as the server-resolved role.
func TestIdentityHeadersAreAuthoritative(t *testing.T) {
	grafana, _ := tunnel.Lookup("grafana")
	got := serveVia(t, grafana, tunnel.Identity{User: "alice", Role: tunnel.RoleViewer}, map[string]string{
		tunnel.HeaderProxyUser: "attacker",
		tunnel.HeaderProxyRole: "Admin",
	})
	if u := got.Get(tunnel.HeaderProxyUser); u != "alice" {
		t.Fatalf("user header = %q, want the server-resolved %q", u, "alice")
	}
	if role := got.Get(tunnel.HeaderProxyRole); role != tunnel.RoleViewer {
		t.Fatalf("role header = %q, want the server-resolved %q - a forged role must not survive", role, tunnel.RoleViewer)
	}
}

// TestIdentityHeadersStrippedForNonAuthProxyApps: Prometheus/Alertmanager don't consume our identity,
// so the headers must not be forwarded at all - including a client-supplied pair.
func TestIdentityHeadersStrippedForNonAuthProxyApps(t *testing.T) {
	prom, _ := tunnel.Lookup("prometheus")
	got := serveVia(t, prom, tunnel.Identity{User: "alice", Role: tunnel.RoleEditor}, map[string]string{
		tunnel.HeaderProxyRole: "Admin",
	})
	if _, ok := got[tunnel.HeaderProxyUser]; ok {
		t.Errorf("%s must not be sent to a non-auth-proxy app", tunnel.HeaderProxyUser)
	}
	if _, ok := got[tunnel.HeaderProxyRole]; ok {
		t.Errorf("%s must not be sent to a non-auth-proxy app (client-supplied value leaked)", tunnel.HeaderProxyRole)
	}
}

// TestGrafanaRotateShortCircuits is the fix for the auth-proxy reload loop: Grafana 13's SPA POSTs
// /api/user/auth-tokens/rotate, and a 401 would make it reload forever. The Proxier must answer it
// itself with a 200 (never touching the agent) and push the grafana_session_expiry cookie into the
// future so the SPA reschedules instead of spinning.
func TestGrafanaRotateShortCircuits(t *testing.T) {
	grafana, _ := tunnel.Lookup("grafana")
	agentHit := false
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentHit = true
		_, _ = w.Write([]byte("ok"))
	}))
	defer agent.Close()

	p := New(execagent.NewPool(strings.TrimPrefix(agent.URL, "http://")), "agent-token", nil)
	req := httptest.NewRequest("POST", "/clusters/c1/proxy/grafana/api/user/auth-tokens/rotate", nil)
	rec := httptest.NewRecorder()
	p.Serve(rec, req, &domain.Cluster{ID: "c1"}, grafana, []byte("kc"), tunnel.Identity{User: "alice", Role: tunnel.RoleEditor})

	if agentHit {
		t.Fatal("rotate must be short-circuited by the Proxier, not forwarded to the agent")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a 401 makes the SPA reload-loop)", rec.Code)
	}
	var expiry *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "grafana_session_expiry" {
			expiry = ck
		}
	}
	if expiry == nil {
		t.Fatal("missing grafana_session_expiry cookie - the SPA needs it to reschedule rotation")
	}
	if expiry.HttpOnly {
		t.Error("grafana_session_expiry must not be HttpOnly - the SPA reads it via document.cookie")
	}
	if want := tunnel.RoutePrefix("c1", "grafana"); expiry.Path != want {
		t.Errorf("cookie Path = %q, want the app route prefix %q so it overwrites Grafana's own", expiry.Path, want)
	}
	if ts, err := strconv.ParseInt(expiry.Value, 10, 64); err != nil || ts <= time.Now().Unix() {
		t.Errorf("cookie value = %q, want a future unix timestamp so the SPA doesn't immediately re-rotate", expiry.Value)
	}
}

// TestNonGrafanaRotateIsProxied guards the gate: the short-circuit is auth-proxy-only, so the same path
// on a non-auth-proxy app (Prometheus) is a normal proxied request, not a synthesized 200.
func TestNonGrafanaRotateIsProxied(t *testing.T) {
	prom, _ := tunnel.Lookup("prometheus")
	agentHit := false
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentHit = true
		_, _ = w.Write([]byte("ok"))
	}))
	defer agent.Close()

	p := New(execagent.NewPool(strings.TrimPrefix(agent.URL, "http://")), "agent-token", nil)
	req := httptest.NewRequest("POST", "/clusters/c1/proxy/prometheus/api/user/auth-tokens/rotate", nil)
	p.Serve(httptest.NewRecorder(), req, &domain.Cluster{ID: "c1"}, prom, []byte("kc"), tunnel.Identity{})

	if !agentHit {
		t.Fatal("non-auth-proxy app must be proxied to the agent, not short-circuited")
	}
}

// TestForwardsTargetAndPath checks the agent gets the target service coordinates and the /api-prefixed
// app path it must forward on.
func TestForwardsTargetAndPath(t *testing.T) {
	grafana, _ := tunnel.Lookup("grafana")
	var gotPath string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if got := r.Header.Get("X-Kaas-Service"); got != grafana.Service {
			t.Errorf("X-Kaas-Service = %q, want %q", got, grafana.Service)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Errorf("agent auth = %q", got)
		}
	}))
	defer agent.Close()

	p := New(execagent.NewPool(strings.TrimPrefix(agent.URL, "http://")), "agent-token", nil)
	req := httptest.NewRequest("GET", "/clusters/c1/proxy/grafana/public/x.js", nil)
	p.Serve(httptest.NewRecorder(), req, &domain.Cluster{ID: "c1"}, grafana, []byte("kc"), tunnel.Identity{})

	if want := "/http-proxy/api/clusters/c1/proxy/grafana/public/x.js"; gotPath != want {
		t.Fatalf("agent path = %q, want %q", gotPath, want)
	}
}
