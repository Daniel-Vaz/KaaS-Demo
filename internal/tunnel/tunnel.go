// Package tunnel is the request-driven seam behind the Monitoring page's "Open UI" links. The
// kube-prometheus-stack add-on deploys Grafana, Prometheus and Alertmanager with their own web UIs,
// but the platform installs no ingress or gateway controller, so those UIs have no external address.
// This seam gives a browser a same-origin route to them: a per-cluster HTTP reverse proxy that rides
// the same path every other cluster-query seam uses - API → host-networked exec agent → the API
// server's `services/<svc>:<port>/proxy` endpoint (see docs/networking.md). No ingress, no new
// API↔agent transport: it is the Monitoring seam's proxy generalised from one `kubectl get --raw`
// into a full streaming HTTP proxy.
//
// The one hard problem any subpath proxy has is that these apps emit ABSOLUTE-path asset URLs
// (`/public/...`, `/graph`). We solve it not by rewriting responses (fragile - Grafana builds URLs
// in JS) but by configuring each app's route-prefix AT INSTALL to the tunnel path, keyed on the
// cluster ID (known at add-on-install time). RoutePrefix is that single source of truth, shared by
// the catalog values templating and the agent's proxy target so the two never drift.
//
// Access is view-scoped like the Monitoring page, and the proxy runs with the cluster ADMIN
// kubeconfig server-side (the `view` role can't `get services/proxy`); production would mint a
// per-app scoped token instead. The browser never sees the kubeconfig - it rides the internal
// API→agent hop only.
package tunnel

import (
	"net/http"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// PublicPrefix is the browser-facing path prefix the SPA reaches the API under: nginx rewrites
// `^/api/` to the API root (deploy/nginx.conf), and the Vite dev proxy and the Helm chart use it
// too. The proxied UIs are configured with a route-prefix that INCLUDES it (RoutePrefix), because
// the absolute paths a browser requests carry it - so the agent must forward the /api-prefixed path
// through to the app, and the app must serve/link under it.
const PublicPrefix = "/api"

// App-level roles the proxied UI grants a tunnel user. They are Grafana's org-role names, mapped
// from the platform's own read/write access (see the Proxier contract): a read-role group-mate gets
// Viewer, an owner/admin/write-role group-mate gets Editor. Editor - not Admin - is deliberate:
// "write on the cluster" means editing dashboards, not administering Grafana's users and orgs.
const (
	RoleViewer = "Viewer"
	RoleEditor = "Editor"
)

// Auth-proxy headers. Grafana is configured at install to authenticate from these
// ([auth.proxy] header_name/headers, see internal/catalog), so a tunnel user is signed in as their
// portal identity with their portal-derived role and never sees a login screen or a shared password.
//
// SECURITY: these are trusted by the app, so they MUST be authoritative - a Proxier sets them from
// the server-resolved actor and must overwrite/remove any client-supplied copy, or a user could
// forge their own role. Grafana accepts them only because this tunnel is the sole network route to
// it (no ingress); production would additionally pin [auth.proxy] whitelist to the proxy's address.
const (
	HeaderProxyUser = "X-Webauth-User"
	HeaderProxyRole = "X-Webauth-Role"
)

// App is one proxied in-cluster web UI. Namespace/Service/Port are the coordinates the agent turns
// into an API-server `services/<Service>:<Port>/proxy` target. The service names are
// kube-prometheus-stack's, whose release name is the add-on name (internal/monitoring.AddonName).
type App struct {
	ID        string `json:"id"`   // URL segment and add-on route-prefix key: grafana|prometheus|alertmanager
	Name      string `json:"name"` // display label for the portal link
	Namespace string `json:"-"`
	Service   string `json:"-"`
	Port      string `json:"-"`
	// WriteScoped marks a UI whose own surface can MUTATE the cluster's monitoring state and that has
	// no user model of its own to express our read/write split - so the only way to honour the split is
	// to gate the whole app on write access. Alertmanager is the case: it ships no auth at all and its
	// UI creates and expires silences, so a read-role group-mate could otherwise mute a cluster's
	// alerts. The portal hides the link for actors who lack write; the API is the authoritative gate.
	WriteScoped bool `json:"write_scoped"`
	// AuthProxy marks a UI that authenticates from our injected identity headers (HeaderProxyUser /
	// HeaderProxyRole) rather than its own login. Only Grafana supports this; Prometheus and
	// Alertmanager have no user model, which is why their access is all-or-nothing (WriteScoped).
	AuthProxy bool `json:"-"`
	// Surface is the portal page that advertises this app's link: SurfaceMonitoring for the
	// kube-prometheus-stack UIs, SurfaceStorage for Longhorn. The tunnel serves them all identically -
	// this only says where the link belongs, so the portal can ask for one page's apps rather than
	// filtering a list it has to understand.
	Surface string `json:"surface"`
	// SelfPrefixed marks a UI that was configured AT INSTALL to serve itself under its tunnel path
	// (RoutePrefix) - Grafana's serve_from_sub_path, Prometheus/Alertmanager's routePrefix. Those apps
	// emit absolute URLs that already carry the prefix, so the agent strips the API server's internal
	// rewrite and stops there.
	//
	// An app that CANNOT be told its base path (longhorn-ui takes no such setting) emits absolute URLs
	// rooted at "/". For those the agent substitutes the browser-facing prefix instead of stripping to
	// nothing, AND injects a client-side shim that re-bases the URLs the app's JavaScript builds at
	// runtime - which no response rewriting can reach. See internal/shell/agent/basepath.go.
	SelfPrefixed bool `json:"-"`
}

// Portal surfaces an app's link is advertised on.
const (
	SurfaceMonitoring = "monitoring"
	SurfaceStorage    = "storage"
)

// AppsFor is the registry filtered to one portal surface.
func AppsFor(surface string) []App {
	out := make([]App, 0, len(Apps))
	for _, a := range Apps {
		if a.Surface == surface {
			out = append(out, a)
		}
	}
	return out
}

// Apps is the fixed registry of proxied in-cluster UIs - the three kube-prometheus-stack ships, and
// Longhorn's. The portal page named by each app's Surface advertises it so one link per app is
// rendered; the agent proxy resolves a request's {app} segment to its service coordinates here.
var Apps = []App{
	// Grafana carries a real user model, so our role rides in as an auth-proxy identity: Read → Viewer,
	// Write → Editor, enforced by Grafana itself. No login screen, no shared admin password.
	{ID: "grafana", Name: "Grafana", Namespace: "monitoring-system", Service: "kube-prometheus-stack-grafana", Port: "80",
		AuthProxy: true, Surface: SurfaceMonitoring, SelfPrefixed: true},
	// Prometheus is read-only in practice (the operator leaves enableAdminAPI false, so it exposes no
	// mutating endpoint) - safe to open for view access.
	{ID: "prometheus", Name: "Prometheus", Namespace: "monitoring-system", Service: "kube-prometheus-stack-prometheus", Port: "9090",
		Surface: SurfaceMonitoring, SelfPrefixed: true},
	// Alertmanager's UI silences alerts and it ships no auth - write access only. See WriteScoped.
	{ID: "alertmanager", Name: "Alertmanager", Namespace: "monitoring-system", Service: "kube-prometheus-stack-alertmanager", Port: "9093",
		WriteScoped: true, Surface: SurfaceMonitoring, SelfPrefixed: true},
	// Longhorn's UI shows the storage pool from the inside: each node's disks and their free space,
	// every volume with its replicas and health, snapshots. It belongs to the Storage page, which is
	// where the same cluster's PVCs and StorageClasses already are.
	//
	// WriteScoped, for the same reason as Alertmanager and more sharply: longhorn-ui ships NO auth of
	// its own and it deletes volumes, so there is no way to express our read/write split inside the
	// app - a read-role group-mate must not reach it at all.
	//
	// NOT SelfPrefixed: longhorn-ui has no route-prefix/base-path setting to configure at install, so
	// the agent re-bases its absolute URLs onto the tunnel path - in the response for the document,
	// and via an injected shim for the ones its JavaScript builds at runtime. See App.SelfPrefixed.
	{ID: "longhorn", Name: "Longhorn", Namespace: "longhorn-system", Service: "longhorn-frontend", Port: "80",
		WriteScoped: true, Surface: SurfaceStorage},
}

// Identity is the portal actor a tunnel request is served for, and the app-level role they should
// get. Resolved server-side from the actor's access to the cluster; never from the request.
type Identity struct {
	User string // the portal username the app signs the user in as
	Role string // RoleViewer | RoleEditor
}

// Lookup resolves an {app} URL segment to its registry entry.
func Lookup(id string) (App, bool) {
	for _, a := range Apps {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// RoutePrefix is the browser-facing base path a cluster's app UI is served under, e.g.
// "/api/clusters/<id>/proxy/grafana". It is the single source of truth for the subpath: baked into
// the add-on's route-prefix at install (so the app emits assets under it) AND used by the agent to
// reconstruct the path it forwards through the API server's service proxy. Callers that only need
// the segment after PublicPrefix (the API sees the path with /api already stripped by nginx) trim it.
func RoutePrefix(clusterID, appID string) string {
	return PublicPrefix + "/clusters/" + clusterID + "/proxy/" + appID
}

// Proxier serves one HTTP request against a cluster's app UI. It writes the whole response itself
// (status, headers, streamed body, WebSocket upgrade), so callers do their gating BEFORE calling it
// and report nothing afterwards. kc is the cluster admin kubeconfig; app names the target UI; id is
// the server-resolved actor identity an AuthProxy app is signed in as.
//
// Implementations MUST treat id as authoritative: set HeaderProxyUser/HeaderProxyRole from it and
// strip any client-supplied copy, so a user cannot forge a higher role.
type Proxier interface {
	Serve(w http.ResponseWriter, r *http.Request, c *domain.Cluster, app App, kc []byte, id Identity)
}
