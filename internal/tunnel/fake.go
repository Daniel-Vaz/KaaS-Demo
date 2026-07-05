package tunnel

import (
	"fmt"
	"html"
	"net/http"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Fake is the KAAS_TUNNEL=fake Proxier: it serves a small synthesized landing page instead of
// proxying to a real in-cluster UI, so `make up-fake` (no KVM, no real cluster) still demos the
// Monitoring page's "Open UI" links end to end - the link opens a new tab that clearly stands in for
// Grafana/Prometheus/Alertmanager on that cluster.
type Fake struct{}

// NewFake returns the synthesized tunnel backend.
func NewFake() *Fake { return &Fake{} }

func (Fake) Serve(w http.ResponseWriter, r *http.Request, c *domain.Cluster, app App, _ []byte, id Identity) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Echo the resolved identity/role so the fake demonstrates the same role mapping the real proxy
	// hands Grafana - a read-role member lands here as Viewer, an owner/write member as Editor.
	who := id.User + " (" + id.Role + ")"
	fmt.Fprintf(w, fakePage,
		html.EscapeString(app.Name),
		html.EscapeString(app.Name),
		html.EscapeString(c.Name),
		html.EscapeString(app.Service),
		html.EscapeString(app.Namespace),
		html.EscapeString(app.Port),
		html.EscapeString(who),
	)
}

// fakePage is a self-contained stand-in served in fake mode. In real mode the browser would land on
// the actual app UI proxied through the API server's service proxy.
const fakePage = `<!doctype html>
<html><head><meta charset="utf-8"><title>%s (demo tunnel)</title>
<style>
 body{font-family:system-ui,sans-serif;background:#0b0f19;color:#e6e9ef;margin:0;
   display:flex;min-height:100vh;align-items:center;justify-content:center}
 .card{max-width:34rem;padding:2.5rem;border:1px solid #263041;border-radius:14px;background:#111726}
 h1{margin:0 0 .25rem;font-size:1.5rem}
 .sub{color:#8b93a7;margin:0 0 1.5rem}
 dl{display:grid;grid-template-columns:auto 1fr;gap:.35rem 1rem;margin:0;font-size:.9rem}
 dt{color:#8b93a7} dd{margin:0;font-family:ui-monospace,monospace}
 .note{margin-top:1.5rem;font-size:.8rem;color:#6b7488;line-height:1.5}
</style></head>
<body><div class="card">
 <h1>%s</h1>
 <p class="sub">Demo tunnel - cluster <strong>%s</strong></p>
 <dl>
   <dt>Service</dt><dd>%s</dd>
   <dt>Namespace</dt><dd>%s</dd>
   <dt>Port</dt><dd>%s</dd>
   <dt>Signed in as</dt><dd>%s</dd>
 </dl>
 <p class="note">This is the fake (KAAS_TUNNEL=fake) stand-in. In real mode this tab would show the
 live web UI, reverse-proxied through the API server's service proxy - no ingress or gateway
 controller required.</p>
</div></body></html>`
