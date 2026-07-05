package tunnel

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func TestLookup(t *testing.T) {
	for _, id := range []string{"grafana", "prometheus", "alertmanager"} {
		if _, ok := Lookup(id); !ok {
			t.Fatalf("Lookup(%q) not found", id)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatalf("Lookup(nope) should not be found")
	}
}

func TestRoutePrefix(t *testing.T) {
	got := RoutePrefix("abc123", "grafana")
	if want := "/api/clusters/abc123/proxy/grafana"; got != want {
		t.Fatalf("RoutePrefix = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, PublicPrefix) {
		t.Fatalf("RoutePrefix must start with PublicPrefix %q", PublicPrefix)
	}
}

func TestFakeServesLandingPage(t *testing.T) {
	app, _ := Lookup("grafana")
	c := &domain.Cluster{Name: "demo"}
	req := httptest.NewRequest("GET", "/clusters/x/proxy/grafana/", nil)
	rec := httptest.NewRecorder()
	NewFake().Serve(rec, req, c, app, nil, Identity{User: "alice", Role: RoleViewer})
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Grafana") || !strings.Contains(body, "demo") {
		t.Fatalf("landing page missing app/cluster name: %s", body)
	}
	if !strings.Contains(body, "alice") || !strings.Contains(body, RoleViewer) {
		t.Fatalf("landing page should echo the resolved identity/role: %s", body)
	}
}

// TestAppScoping pins the access model each app is registered with: Alertmanager mutates (silences)
// and has no auth of its own, so it must be write-scoped; Prometheus is read-only in practice and
// Grafana carries our role via auth-proxy, so both stay open to view access.
func TestAppScoping(t *testing.T) {
	for _, tc := range []struct {
		id          string
		writeScoped bool
		authProxy   bool
	}{
		{"grafana", false, true},
		{"prometheus", false, false},
		{"alertmanager", true, false},
	} {
		app, ok := Lookup(tc.id)
		if !ok {
			t.Fatalf("Lookup(%q) not found", tc.id)
		}
		if app.WriteScoped != tc.writeScoped {
			t.Errorf("%s WriteScoped = %v, want %v", tc.id, app.WriteScoped, tc.writeScoped)
		}
		if app.AuthProxy != tc.authProxy {
			t.Errorf("%s AuthProxy = %v, want %v", tc.id, app.AuthProxy, tc.authProxy)
		}
	}
}
