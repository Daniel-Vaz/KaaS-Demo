// Package api is the control-plane HTTP surface (REST + SSE). Standard-library only;
// swap in chi/echo later if routing grows. The live event stream (SSE) tailing the
// events broker is what makes provisioning progress visible in the UI. The UI itself is
// the separate web portal (web/portal), served by its own nginx container which reverse-proxies
// here - this process serves only JSON + SSE.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/app"
	"github.com/Daniel-Vaz/KaaS-demo/internal/audit"
	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	"github.com/Daniel-Vaz/KaaS-demo/internal/monitoring"
	"github.com/Daniel-Vaz/KaaS-demo/internal/security"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
	"github.com/Daniel-Vaz/KaaS-demo/internal/tunnel"
	"github.com/Daniel-Vaz/KaaS-demo/internal/version"
	"github.com/coder/websocket"
)

type Server struct {
	app *app.App
	log *slog.Logger
}

func NewServer(a *app.App, log *slog.Logger) *Server { return &Server{app: a, log: log} }

// sessionCookie is the name of the HttpOnly cookie carrying the signed session token.
const sessionCookie = "kaas_session"

// Routes returns the HTTP handler (Go 1.22+ method+pattern routing), wrapped in the auth
// middleware. All routes except the public allowlist (healthz, version, register, login, logout)
// require a valid session; the resolved actor is threaded through the request context (see
// actorFrom).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	// Which release this deployment is running (internal/version, stamped at build time). Public
	// for the same reason /healthz is - it answers a question about the process, not about any
	// tenant - and the payload is deliberately just version/commit/date. The portal footer reads it.
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, version.Get())
	})

	// Auth / session.
	mux.HandleFunc("POST /auth/register", s.register)
	mux.HandleFunc("GET /auth/config", s.authConfig)
	mux.HandleFunc("POST /auth/login", s.login)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("GET /auth/me", s.me)
	mux.HandleFunc("GET /auth/profile", s.profile)

	// Admin: user management.
	mux.HandleFunc("GET /users", s.listUsers)
	mux.HandleFunc("PATCH /users/{id}", s.updateUser)
	mux.HandleFunc("DELETE /users/{id}", s.deleteUser)

	// Admin: group management.
	mux.HandleFunc("POST /groups", s.createGroup)
	mux.HandleFunc("GET /groups", s.listGroups)
	mux.HandleFunc("PATCH /groups/{id}", s.renameGroup)
	mux.HandleFunc("DELETE /groups/{id}", s.deleteGroup)

	mux.HandleFunc("GET /catalog", s.getCatalog)
	mux.HandleFunc("GET /catalog/addons/{name}/values", s.getCatalogAddonValues)

	// Per-user custom add-on catalogs (owner + group-shared, like clusters). See internal/app/customcatalog.go.
	mux.HandleFunc("GET /custom-catalogs", s.listCustomCatalogs)
	mux.HandleFunc("POST /custom-catalogs", s.createCustomCatalog)
	mux.HandleFunc("POST /custom-catalogs/chart-values", s.fetchChartValues)
	mux.HandleFunc("GET /custom-catalogs/{id}", s.getCustomCatalog)
	mux.HandleFunc("PATCH /custom-catalogs/{id}", s.renameCustomCatalog)
	mux.HandleFunc("DELETE /custom-catalogs/{id}", s.deleteCustomCatalog)
	mux.HandleFunc("POST /custom-catalogs/{id}/addons", s.addCustomAddon)
	mux.HandleFunc("PUT /custom-catalogs/{id}/addons/{name}", s.updateCustomAddon)
	mux.HandleFunc("DELETE /custom-catalogs/{id}/addons/{name}", s.removeCustomAddon)

	mux.HandleFunc("GET /capacity", s.getCapacity)
	mux.HandleFunc("POST /clusters", s.createCluster)
	mux.HandleFunc("GET /clusters", s.listClusters)
	mux.HandleFunc("GET /clusters/{id}", s.getCluster)
	mux.HandleFunc("PATCH /clusters/{id}", s.updateCluster)
	// Extra node disks. Narrow add/remove rather than a whole-list PATCH like node pools: a disk
	// belongs to ONE node, and a lost update on a whole-list replace would silently destroy another
	// disk - and with it, its data. See internal/app/nodedisk.go.
	mux.HandleFunc("POST /clusters/{id}/disks", s.addNodeDisk)
	mux.HandleFunc("DELETE /clusters/{id}/nodes/{vm}/disks/{disk}", s.removeNodeDisk)
	mux.HandleFunc("DELETE /clusters/{id}", s.deleteCluster)
	mux.HandleFunc("GET /clusters/{id}/events", s.streamEvents)
	mux.HandleFunc("GET /clusters/{id}/shell", s.streamShell)
	mux.HandleFunc("GET /clusters/{id}/nodes/{vm}/ssh", s.streamNodeSSH)
	mux.HandleFunc("GET /clusters/{id}/kubeconfig", s.getKubeconfig)
	mux.HandleFunc("GET /clusters/{id}/upgrades", s.getUpgrades)
	mux.HandleFunc("POST /clusters/{id}/upgrades", s.promoteCluster)
	mux.HandleFunc("GET /clusters/{id}/operations", s.getOperations)
	mux.HandleFunc("GET /clusters/{id}/addons/{name}/values", s.getClusterAddonValues)
	mux.HandleFunc("PUT /clusters/{id}/addons/{name}/values", s.setClusterAddonValues)
	mux.HandleFunc("GET /clusters/{id}/metrics", s.getMetrics)
	mux.HandleFunc("GET /clusters/{id}/health", s.getHealth)

	// Workloads page: the request-driven kube query seam (internal/kube). All are view-scoped except
	// scale (write-scoped); logs is a WebSocket that streams `kubectl logs -f`.
	mux.HandleFunc("GET /clusters/{id}/namespaces", s.getNamespaces)
	mux.HandleFunc("GET /clusters/{id}/workloads", s.listWorkloads)
	mux.HandleFunc("GET /clusters/{id}/workloads/{kind}/{namespace}/{name}", s.getWorkload)
	mux.HandleFunc("GET /clusters/{id}/workloads/{kind}/{namespace}/{name}/manifest", s.getWorkloadManifest)
	mux.HandleFunc("GET /clusters/{id}/workloads/{kind}/{namespace}/{name}/events", s.getWorkloadEvents)
	mux.HandleFunc("POST /clusters/{id}/workloads/{kind}/{namespace}/{name}/scale", s.scaleWorkload)
	mux.HandleFunc("GET /clusters/{id}/workloads/{kind}/{namespace}/{name}/logs", s.streamWorkloadLogs)

	// Storage page: the same kube query seam, storage half (internal/kube/storage.go). All read-only
	// and view-scoped. A claim's name is a path segment; a StorageClass is cluster-scoped, so its name
	// alone identifies it.
	mux.HandleFunc("GET /clusters/{id}/storage/pvcs", s.listPVCs)
	mux.HandleFunc("GET /clusters/{id}/storage/pvcs/{namespace}/{name}", s.getPVC)
	mux.HandleFunc("GET /clusters/{id}/storage/pvcs/{namespace}/{name}/manifest", s.getPVCManifest)
	mux.HandleFunc("GET /clusters/{id}/storage/pvcs/{namespace}/{name}/events", s.getPVCEvents)
	mux.HandleFunc("GET /clusters/{id}/storage/apps", s.getStorageApps)
	mux.HandleFunc("GET /clusters/{id}/storage/storageclasses", s.listStorageClasses)
	mux.HandleFunc("GET /clusters/{id}/storage/storageclasses/{name}/manifest", s.getStorageClassManifest)

	// Networking page: the same kube query seam, network half (internal/kube/network.go). All
	// read-only and view-scoped. The overview carries the cluster's north-south contract (gateway
	// address + wildcard DNS) and the apps published through it; the rest are the raw objects. Routes
	// are keyed by kind as well as namespace/name - the Gateway API route kinds are distinct
	// resources sharing one page.
	mux.HandleFunc("GET /clusters/{id}/networking/overview", s.getNetworkOverview)
	mux.HandleFunc("GET /clusters/{id}/networking/services", s.listServices)
	mux.HandleFunc("GET /clusters/{id}/networking/services/{namespace}/{name}", s.getService)
	mux.HandleFunc("GET /clusters/{id}/networking/services/{namespace}/{name}/manifest", s.getServiceManifest)
	mux.HandleFunc("GET /clusters/{id}/networking/services/{namespace}/{name}/events", s.getServiceEvents)
	mux.HandleFunc("GET /clusters/{id}/networking/gateways", s.listGateways)
	mux.HandleFunc("GET /clusters/{id}/networking/gateways/{namespace}/{name}/manifest", s.getGatewayManifest)
	mux.HandleFunc("GET /clusters/{id}/networking/routes", s.listRoutes)
	mux.HandleFunc("GET /clusters/{id}/networking/routes/{kind}/{namespace}/{name}/manifest", s.getRouteManifest)

	// Secrets page: the same kube query seam, config half (internal/kube/config.go). ConfigMaps are
	// view-scoped on the actor's own reader credential; Secrets are view-scoped too but read with the
	// admin kubeconfig server-side and returned REDACTED (keys + byte lengths, never values). The
	// vault-session endpoint mints the "View in Vault" handoff token.
	mux.HandleFunc("GET /clusters/{id}/configmaps", s.listConfigMaps)
	mux.HandleFunc("GET /clusters/{id}/configmaps/{namespace}/{name}", s.getConfigMap)
	mux.HandleFunc("GET /clusters/{id}/configmaps/{namespace}/{name}/manifest", s.getConfigMapManifest)
	mux.HandleFunc("GET /clusters/{id}/secrets", s.listSecrets)
	mux.HandleFunc("GET /clusters/{id}/secrets/{namespace}/{name}", s.getSecret)
	mux.HandleFunc("GET /clusters/{id}/secrets/{namespace}/{name}/manifest", s.getSecretManifest)
	mux.HandleFunc("GET /clusters/{id}/vault-session", s.getVaultSession)

	// Monitoring page: the request-driven PromQL query seam (internal/monitoring). View-scoped.
	mux.HandleFunc("GET /clusters/{id}/monitoring", s.getMonitoringTabs)
	mux.HandleFunc("GET /clusters/{id}/monitoring/{tab}", s.getMonitoringTab)

	// Monitoring "Open UI" tunnel (internal/tunnel): a reverse proxy to a cluster's in-cluster
	// Grafana/Prometheus/Alertmanager, all HTTP methods. View-scoped, admin kubeconfig server-side.
	// The bare-app path redirects to the trailing slash the app UIs expect as their root.
	mux.HandleFunc("/clusters/{id}/proxy/{app}/{rest...}", s.tunnel)
	mux.HandleFunc("/clusters/{id}/proxy/{app}", s.tunnelRedirect)

	// Security page: the request-driven Trivy CRD query seam (internal/security). View-scoped.
	mux.HandleFunc("GET /clusters/{id}/security", s.getSecurityMeta)
	mux.HandleFunc("GET /clusters/{id}/security/overview", s.getSecurityOverview)
	mux.HandleFunc("GET /clusters/{id}/security/reports/{kind}", s.listSecurityReports)
	mux.HandleFunc("GET /clusters/{id}/security/report/{kind}", s.getSecurityReport)

	// Audit tab: the request-driven API-server audit query seam (internal/audit). View-scoped.
	mux.HandleFunc("GET /clusters/{id}/audit", s.getAuditEvents)
	return s.withAuth(mux)
}

type actorCtxKey struct{}

// withAuth resolves the session cookie into an actor and puts it on the request context. Public
// routes (login, register, logout, healthz, version) pass through; everything else 401s without a valid
// session. SSE and the shell WebSocket ride the same same-origin cookie, so they authenticate here
// too - no header plumbing needed.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublic(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		actor := s.resolveActor(r)
		if actor == nil {
			writeErr(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorCtxKey{}, actor)))
	})
}

// isPublic reports whether a route is reachable without a session.
func isPublic(method, path string) bool {
	switch path {
	case "/healthz", "/version":
		return method == http.MethodGet
	// /auth/config is public because it is what the login page reads BEFORE it can authenticate -
	// to know whether to offer a register form at all. It exposes only which mechanism is
	// configured, never the directory's address, base DN or mapping rules. (The other capability
	// channel, /catalog, is session-gated, which is why this needs its own route.)
	case "/auth/config":
		return method == http.MethodGet
	case "/auth/register", "/auth/login", "/auth/logout":
		return method == http.MethodPost
	}
	return false
}

// clientIP is the caller's address, for the login throttle only.
//
// X-Forwarded-For's leftmost entry is the original client as recorded by the first proxy - here,
// the portal's nginx (see deploy/nginx.conf). It is CLIENT-CONTROLLED: anyone can send the header
// directly to the API and choose their own key. That is tolerable precisely because the throttle
// does not depend on it - the per-username counter is the half that protects directory accounts,
// and it cannot be rotated away from. This scope only catches the cruder "one host, many usernames"
// spray. Never use this for authorization.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// resolveActor loads the user named by a valid session cookie, or nil.
func (s *Server) resolveActor(r *http.Request) *domain.User {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	u, err := s.app.ResolveSession(ck.Value)
	if err != nil {
		return nil
	}
	return u
}

// actorFrom returns the authenticated actor put on the context by withAuth (never nil on a
// protected route, since withAuth 401s first).
func actorFrom(r *http.Request) *domain.User {
	a, _ := r.Context().Value(actorCtxKey{}).(*domain.User)
	return a
}

// requestIsHTTPS reports whether the request reached the API over TLS - directly, or via a proxy
// that terminated it and said so. The local demo runs plain HTTP behind nginx with nothing setting
// this header, so Secure stays off there (a Secure cookie is simply dropped by the browser over
// http://, which would break login) - production terminates TLS at the edge and sets it.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setSessionCookie issues a signed session cookie for userID. Secure follows the request scheme
// (requestIsHTTPS) rather than a hardcoded true or false: the local demo terminates at nginx over
// plain HTTP with nothing setting X-Forwarded-Proto, and a Secure cookie is silently dropped by the
// browser over http://, which would break every login - but it flips to Secure automatically the
// moment TLS reaches the API, directly or via a proxy that says so.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, userID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.app.IssueSession(userID),
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(app.SessionTTL.Seconds()),
	})
}

// clearSessionCookie expires the session cookie (logout). Secure mirrors setSessionCookie's - it
// must match the cookie it is expiring, or a plain-HTTP browser would keep the session.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// getMetrics returns the latest resource-usage snapshot for a cluster (per-node CPU/memory).
// A 204 (no content) means the cluster exists but has no snapshot yet - it just became Ready, or
// metrics-server is disabled; the portal renders that as "gathering metrics" / hides the panels.
func (s *Server) getMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap, err := s.app.Metrics(actorFrom(r), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Both "no such cluster" and "no snapshot yet" surface as ErrNotFound. Re-check the
			// cluster (owner-scoped) to tell them apart: a live cluster with no snapshot is 204, a
			// missing or cross-tenant one 404.
			if _, cerr := s.app.GetCluster(actorFrom(r), id); cerr == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// getHealth returns the latest health snapshot for a cluster (per-check status + per-node detail).
// Same shape as getMetrics: a 204 (no content) means the cluster exists but has no snapshot yet -
// it just became Ready, or the health checker is disabled; the portal renders that as "evaluating".
func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap, err := s.app.Health(actorFrom(r), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Both "no such cluster" and "no snapshot yet" surface as ErrNotFound. Re-check the
			// cluster (owner-scoped) to tell them apart: a live cluster with no snapshot is 204, a
			// missing or cross-tenant one 404.
			if _, cerr := s.app.GetCluster(actorFrom(r), id); cerr == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// getOperations returns a cluster's action history (create, scale, add-on, upgrade), newest first.
func (s *Server) getOperations(w http.ResponseWriter, r *http.Request) {
	ops, err := s.app.Operations(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ops)
}

// getCatalog exposes the version/release catalog so the UI wizard can render OS,
// Kubernetes, add-on, and bundle choices - plus the enabled infrastructure providers, which the
// wizard needs for the same reason (they are the choices available at create time). The provider
// list rides on this payload rather than a separate endpoint because the wizard already fetches
// and caches it, and a provider's network shape is as much "what can I ask for" as a bundle is.
// bundle_addons_optional rides along for the same reason: it decides whether the wizard's bundled
// add-on cards are locked on. It is advisory to the portal only - CreateCluster enforces it.
func (s *Server) getCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		*catalog.Catalog
		Providers            []app.ProviderInfo `json:"providers"`
		BundleAddonsOptional bool               `json:"bundle_addons_optional"`
	}{s.app.Catalog, s.app.Providers(), s.app.BundleAddonsOptional})
}

// getUpgrades lists the release bundles a cluster can be promoted to.
func (s *Server) getUpgrades(w http.ResponseWriter, r *http.Request) {
	ups, err := s.app.Upgrades(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ups)
}

// getCatalogAddonValues returns the values-editor payload for a catalog add-on during cluster
// creation (chart defaults + curated catalog overrides + the two merged). The optional ?bundle
// pins the add-on's version to the chosen bundle.
func (s *Server) getCatalogAddonValues(w http.ResponseWriter, r *http.Request) {
	view, err := s.app.AddonValues(r.Context(), actorFrom(r), r.URL.Query().Get("bundle"), r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// getClusterAddonValues returns the values-editor payload for one of a cluster's add-ons, including
// that cluster's saved override and the add-on's phase. Read access suffices.
func (s *Server) getClusterAddonValues(w http.ResponseWriter, r *http.Request) {
	view, err := s.app.ClusterAddonValues(r.Context(), actorFrom(r), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// setClusterAddonValues saves a per-cluster Helm values override for an add-on (write-scoped),
// driving the reconciler to run a helm upgrade. An empty body value resets to catalog defaults.
func (s *Server) setClusterAddonValues(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Values string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.app.SetClusterAddonValues(actorFrom(r), r.PathValue("id"), r.PathValue("name"), req.Values)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err) // 404 cross-tenant / 403 read-only group member
			return
		}
		writeErr(w, http.StatusBadRequest, err) // not Ready, invalid YAML, add-on not installed
		return
	}
	s.writeCluster(w, http.StatusOK, actorFrom(r), c)
}

// promoteCluster triggers an upgrade: it records the desired target bundle; the reconciler
// converges the running cluster toward it (in-place kubeadm for Kubernetes, rolling replacement
// for the OS, helm for add-ons).
func (s *Server) promoteCluster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bundle string `json:"bundle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.app.PromoteCluster(actorFrom(r), r.PathValue("id"), req.Bundle)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err) // 404 cross-tenant / 403 read-only group member
			return
		}
		writeErr(w, http.StatusBadRequest, err) // not Ready, unknown/unreachable bundle, single-node OS upgrade
		return
	}
	s.writeCluster(w, http.StatusOK, actorFrom(r), c)
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	var req app.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFrom(r)
	c, err := s.app.CreateCluster(actor, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, clusterResponse{Cluster: c, OwnerUsername: actor.Username})
}

// getCapacity exposes host budget vs. current allocation so the UI can show headroom.
func (s *Server) getCapacity(w http.ResponseWriter, r *http.Request) {
	rep, err := s.app.Capacity(actorFrom(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// updateCluster applies an in-place edit (scale workers, add/remove add-ons). The reconciler
// converges the running cluster to the new desired state.
func (s *Server) updateCluster(w http.ResponseWriter, r *http.Request) {
	var req app.UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.app.UpdateCluster(actorFrom(r), r.PathValue("id"), req)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err) // 404 cross-tenant / 403 read-only group member
			return
		}
		writeErr(w, http.StatusBadRequest, err) // validation: bad workers, unknown add-on, not editable
		return
	}
	s.writeCluster(w, http.StatusOK, actorFrom(r), c)
}

// addNodeDisk attaches a new extra disk to one worker node (write-scoped; the cluster must be Ready).
func (s *Server) addNodeDisk(w http.ResponseWriter, r *http.Request) {
	var req app.AddNodeDiskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.app.AddNodeDisk(actorFrom(r), r.PathValue("id"), req)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err) // 404 cross-tenant / 403 read-only group member
			return
		}
		writeErr(w, http.StatusBadRequest, err) // validation, quota, or not Ready
		return
	}
	s.writeCluster(w, http.StatusOK, actorFrom(r), c)
}

// removeNodeDisk marks a node's extra disk for removal (write-scoped). THIS DESTROYS THE DISK'S
// DATA; the portal confirms first, but this endpoint is the authoritative gate and does not ask.
func (s *Server) removeNodeDisk(w http.ResponseWriter, r *http.Request) {
	c, err := s.app.RemoveNodeDisk(actorFrom(r), r.PathValue("id"), r.PathValue("vm"), r.PathValue("disk"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.writeCluster(w, http.StatusOK, actorFrom(r), c)
}

// clusterResponse decorates a Cluster with its owner's username (see app.OwnerUsernames) and a
// per-actor CanManage flag, both resolved at serve time. OwnerUsername lets the portal show who
// requested a cluster without an admin-gated users lookup. CanManage tells the portal whether THIS
// actor may mutate it - owner/admin/write-role group-mate - which it can no longer derive from the
// user alone now that a member's role is per-group (the server is the authoritative gate regardless).
type clusterResponse struct {
	*domain.Cluster
	OwnerUsername string `json:"owner_username"`
	CanManage     bool   `json:"can_manage"`
}

// writeCluster resolves a single cluster's owner username and the actor's manage access, and writes
// the decorated response.
func (s *Server) writeCluster(w http.ResponseWriter, status int, actor *domain.User, c *domain.Cluster) {
	names, err := s.app.OwnerUsernames([]*domain.Cluster{c})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, status, clusterResponse{Cluster: c, OwnerUsername: names[c.OwnerID], CanManage: s.app.CanManageCluster(actor, c)})
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	cs, err := s.app.ListClusters(actor)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	names, err := s.app.OwnerUsernames(cs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]clusterResponse, 0, len(cs))
	for _, c := range cs {
		out = append(out, clusterResponse{Cluster: c, OwnerUsername: names[c.OwnerID], CanManage: s.app.CanManageCluster(actor, c)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request) {
	c, err := s.app.GetCluster(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.writeCluster(w, http.StatusOK, actorFrom(r), c)
}

func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteCluster(actorFrom(r), r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) getKubeconfig(w http.ResponseWriter, r *http.Request) {
	// A per-user credential minted for THIS actor: their own identity (CN=username) and their resolved
	// access as the Kubernetes group the cluster binds (writers → cluster-admin, readers → view).
	// DownloadKubeconfig resolves the role. A read-only download and the cert expiry are marked in
	// headers so the portal can label them, without changing the media type.
	kc, readOnly, notAfter, err := s.app.DownloadKubeconfig(r.Context(), actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), fmt.Errorf("kubeconfig not ready: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	if readOnly {
		w.Header().Set("X-Kaas-Kubeconfig-Access", "read-only")
	}
	if !notAfter.IsZero() {
		w.Header().Set("X-Kaas-Kubeconfig-Expires", notAfter.UTC().Format(time.RFC3339))
	}
	w.Write(kc)
}

// streamEvents is Server-Sent Events: the browser subscribes and watches provisioning live.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Owner-or-admin only - a cross-tenant subscription is a 404 (same as reading the cluster).
	if _, err := s.app.GetCluster(actorFrom(r), id); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	history, ch, cancel := s.app.Broker.Subscribe(id)
	defer cancel()

	// Replay history first, then stream live events. History is delivered out-of-band from the
	// channel so a large backlog isn't truncated by the channel buffer (see Broker.Subscribe).
	for _, e := range history {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// streamShell upgrades to a WebSocket and hands the browser an interactive cluster terminal. The
// API can't reach the cluster API server itself (see docs/networking.md); it holds the browser end
// and delegates to the selected shell backend - the in-process fake (synthesized kubectl) or the
// worker proxy (a real bash+kubectl PTY on the host-networked worker). Once upgraded, problems are
// reported as an "error" control frame, since the HTTP response has been taken over.
func (s *Server) streamShell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor := actorFrom(r)
	c, err := s.app.GetCluster(actor, id) // owner-or-admin-or-group-mate; cross-tenant is a 404
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The portal is same-origin behind nginx, and the Vite dev proxy rewrites Host, so we don't
		// enforce an Origin match here. Authn/authorization already happened above via the session
		// cookie (the WebSocket handshake carries it) and the owner-or-admin check.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	ctx := r.Context()
	status, reason := s.RunShellSession(ctx, actor, c, shell.NewConn(ctx, conn))
	_ = conn.Close(status, reason)
}

// streamNodeSSH upgrades to a WebSocket and hands the browser an SSH session as the kaas user on one
// cluster VM (the Nodes tab's SSH button). Unlike the cluster shell it gates on WRITE access, not
// view - see App.NodeSSHTarget for why that is the right boundary and not an escalation. Authorization
// and node lookup happen BEFORE the upgrade so a read-role member gets a clean 403 and an unknown node
// a 404; a node with no IP yet is reported in-terminal after the upgrade, like the shell's not-Ready
// case. Once upgraded, the API is a byte pipe to the selected backend (in-process fake or the
// node-ssh sandbox). Note it does NOT gate on phase Ready: SSH needs only a booted VM with an IP, and
// a half-provisioned cluster is exactly when getting onto the box to read the journal is useful.
func (s *Server) streamNodeSSH(w http.ResponseWriter, r *http.Request) {
	actor := actorFrom(r)
	c, n, err := s.app.NodeSSHTarget(actor, r.PathValue("id"), r.PathValue("vm"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	ctx := r.Context()
	status, reason := s.RunNodeSSHSession(ctx, actor, c, n, shell.NewConn(ctx, conn))
	_ = conn.Close(status, reason)
}

// --- Workloads ---------------------------------------------------------------

// workloadStatusFor maps a workloads error to an HTTP status: a not-Ready cluster is 409, otherwise
// the usual mapping (404 cross-tenant, 403 read-only, 401, 500).
func workloadStatusFor(err error) int {
	if errors.Is(err, app.ErrClusterNotReady) {
		return http.StatusConflict
	}
	return statusFor(err)
}

// workloadRefFrom builds a WorkloadRef from the {kind}/{namespace}/{name} path values, rejecting an
// unknown kind with a 400.
func workloadRefFrom(r *http.Request) (kube.WorkloadRef, error) {
	k, ok := kube.ParseKind(r.PathValue("kind"))
	if !ok {
		return kube.WorkloadRef{}, fmt.Errorf("unknown workload kind %q", r.PathValue("kind"))
	}
	return kube.WorkloadRef{Kind: k, Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}, nil
}

// getNamespaces lists a Ready cluster's namespaces (for the Workloads page namespace picker).
func (s *Server) getNamespaces(w http.ResponseWriter, r *http.Request) {
	ns, err := s.app.WorkloadNamespaces(r.Context(), actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

// listWorkloads lists workloads in the cluster; ?namespace= scopes to one (absent = all namespaces).
func (s *Server) listWorkloads(w http.ResponseWriter, r *http.Request) {
	ws, err := s.app.Workloads(r.Context(), actorFrom(r), r.PathValue("id"), r.URL.Query().Get("namespace"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// getWorkload returns one workload's detail (spec rollup + pods).
func (s *Server) getWorkload(w http.ResponseWriter, r *http.Request) {
	ref, err := workloadRefFrom(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	d, err := s.app.Workload(r.Context(), actorFrom(r), r.PathValue("id"), ref)
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// getWorkloadManifest returns a workload's YAML.
func (s *Server) getWorkloadManifest(w http.ResponseWriter, r *http.Request) {
	ref, err := workloadRefFrom(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	y, err := s.app.WorkloadManifest(r.Context(), actorFrom(r), r.PathValue("id"), ref)
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(y))
}

// getWorkloadEvents returns events for a workload and its owned objects.
func (s *Server) getWorkloadEvents(w http.ResponseWriter, r *http.Request) {
	ref, err := workloadRefFrom(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ev, err := s.app.WorkloadEvents(r.Context(), actorFrom(r), r.PathValue("id"), ref)
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// scaleWorkload sets a workload's replica count (write-scoped: a read-role group member gets 403).
func (s *Server) scaleWorkload(w http.ResponseWriter, r *http.Request) {
	ref, err := workloadRefFrom(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Replicas *int `json:"replicas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Replicas == nil {
		writeErr(w, http.StatusBadRequest, errors.New("replicas is required"))
		return
	}
	if err := s.app.ScaleWorkload(r.Context(), actorFrom(r), r.PathValue("id"), ref, *req.Replicas); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) || errors.Is(err, app.ErrClusterNotReady) {
			writeErr(w, workloadStatusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err) // not scalable, bad replicas, kubectl error
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// streamWorkloadLogs upgrades to a WebSocket and streams a pod's logs (kubectl logs [-f]). Like the
// shell it delegates to the kube seam - the fake streams synthesized lines, the worker proxy bridges
// a real `kubectl logs -f`. Log bytes ride binary frames; a fatal error rides a text control frame.
func (s *Server) streamWorkloadLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor := actorFrom(r)
	// Owner-or-view check before the upgrade, so an unauthorized request gets a clean HTTP status.
	if _, err := s.app.GetCluster(actor, id); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	ctx := r.Context()
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	ref := kube.LogRef{
		Namespace: r.PathValue("namespace"),
		Pod:       r.URL.Query().Get("pod"),
		Container: r.URL.Query().Get("container"),
		TailLines: tail,
		Follow:    r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true",
	}
	status, reason := s.RunLogSession(ctx, actor, id, ref, shell.NewConn(ctx, conn))
	_ = conn.Close(status, reason)
}

// wsLogSink adapts the browser WebSocket to kube.LogSink, forwarding log bytes as binary frames.
type wsLogSink struct{ term shell.Conn }

func (s wsLogSink) Write(p []byte) error { return s.term.WriteBinary(p) }

// --- Storage -----------------------------------------------------------------
//
// The Storage page's endpoints. All read-only and view-scoped, and all reached through the same kube
// seam as Workloads, so they share workloadStatusFor (a not-Ready cluster is a 409).

// pvcRefFrom builds a PVCRef from the {namespace}/{name} path values.
func pvcRefFrom(r *http.Request) kube.PVCRef {
	return kube.PVCRef{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
}

// listPVCs lists the cluster's PersistentVolumeClaims; ?namespace= scopes to one (absent = all).
func (s *Server) listPVCs(w http.ResponseWriter, r *http.Request) {
	pvcs, err := s.app.PVCs(r.Context(), actorFrom(r), r.PathValue("id"), r.URL.Query().Get("namespace"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, pvcs)
}

// getPVC returns one claim's detail (bound PV + mounting pods).
func (s *Server) getPVC(w http.ResponseWriter, r *http.Request) {
	d, err := s.app.PVC(r.Context(), actorFrom(r), r.PathValue("id"), pvcRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// getPVCManifest returns a claim's YAML.
func (s *Server) getPVCManifest(w http.ResponseWriter, r *http.Request) {
	y, err := s.app.PVCManifest(r.Context(), actorFrom(r), r.PathValue("id"), pvcRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(y))
}

// getPVCEvents returns a claim's events.
func (s *Server) getPVCEvents(w http.ResponseWriter, r *http.Request) {
	ev, err := s.app.PVCEvents(r.Context(), actorFrom(r), r.PathValue("id"), pvcRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// getStorageApps returns the in-cluster storage UIs the Storage page can link to (Longhorn), plus
// whether this cluster can actually serve them - so the page renders the link, or the reason it
// cannot, without issuing a proxy request that would 409. Mirrors getMonitoringTabs' Apps field; the
// Storage page has no tab descriptors of its own to hang it off.
func (s *Server) getStorageApps(w http.ResponseWriter, r *http.Request) {
	c, err := s.app.GetCluster(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, storageAppsMeta{
		Enabled: s.app.StorageUIEnabled(c),
		Ready:   c.Phase == domain.PhaseReady,
		Apps:    s.app.TunnelApps(tunnel.SurfaceStorage),
	})
}

type storageAppsMeta struct {
	Enabled bool         `json:"enabled"` // the longhorn add-on is installed
	Ready   bool         `json:"ready"`
	Apps    []tunnel.App `json:"apps"`
}

// listStorageClasses lists the cluster's StorageClasses (each row carries every field the detail view
// shows, so there is no per-class get - only the YAML is fetched on demand).
func (s *Server) listStorageClasses(w http.ResponseWriter, r *http.Request) {
	scs, err := s.app.StorageClasses(r.Context(), actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, scs)
}

// getStorageClassManifest returns a StorageClass's YAML.
func (s *Server) getStorageClassManifest(w http.ResponseWriter, r *http.Request) {
	y, err := s.app.StorageClassManifest(r.Context(), actorFrom(r), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(y))
}

// --- Secrets page (ConfigMaps + Secrets + the Vault handoff) ------------------
//
// The config half of the kube seam (internal/kube/config.go): all read-only, all sharing
// workloadStatusFor (a not-Ready cluster is 409). Secret values are redacted server-side.

// configMapRefFrom / secretRefFrom build a ref from the {namespace}/{name} path values.
func configMapRefFrom(r *http.Request) kube.ConfigMapRef {
	return kube.ConfigMapRef{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
}

func secretRefFrom(r *http.Request) kube.SecretRef {
	return kube.SecretRef{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
}

// listConfigMaps lists the cluster's ConfigMaps; ?namespace= scopes to one (absent = all).
func (s *Server) listConfigMaps(w http.ResponseWriter, r *http.Request) {
	cms, err := s.app.ConfigMaps(r.Context(), actorFrom(r), r.PathValue("id"), r.URL.Query().Get("namespace"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, cms)
}

// getConfigMap returns one ConfigMap's detail (its full data - ConfigMaps are not secret).
func (s *Server) getConfigMap(w http.ResponseWriter, r *http.Request) {
	d, err := s.app.ConfigMap(r.Context(), actorFrom(r), r.PathValue("id"), configMapRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// getConfigMapManifest returns a ConfigMap's YAML.
func (s *Server) getConfigMapManifest(w http.ResponseWriter, r *http.Request) {
	y, err := s.app.ConfigMapManifest(r.Context(), actorFrom(r), r.PathValue("id"), configMapRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(y))
}

// listSecrets lists the cluster's Secrets (keys only, never values); ?namespace= scopes to one.
func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	secs, err := s.app.ClusterSecrets(r.Context(), actorFrom(r), r.PathValue("id"), r.URL.Query().Get("namespace"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, secs)
}

// getSecret returns one Secret's detail with every value REDACTED (keys + byte lengths only).
func (s *Server) getSecret(w http.ResponseWriter, r *http.Request) {
	d, err := s.app.ClusterSecret(r.Context(), actorFrom(r), r.PathValue("id"), secretRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// getSecretManifest returns a Secret's YAML with the data values scrubbed.
func (s *Server) getSecretManifest(w http.ResponseWriter, r *http.Request) {
	y, err := s.app.ClusterSecretManifest(r.Context(), actorFrom(r), r.PathValue("id"), secretRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(y))
}

// getVaultSession mints the "View in Vault" handoff: a short-lived, access-scoped Vault token plus the
// UI URL for the cluster's path. A cluster whose Vault path isn't provisioned yet is a 409.
func (s *Server) getVaultSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.app.VaultSession(r.Context(), actorFrom(r), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, app.ErrVaultNotWired) {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// --- Networking ---------------------------------------------------------------
//
// The Networking page's endpoints. All read-only and view-scoped, and all reached through the same
// kube seam as Workloads/Storage, so they share workloadStatusFor (a not-Ready cluster is a 409).
// There is no add-on gate: a cluster without the Gateway API still has Services, and its platform
// contract (the reserved gateway address, the wildcard record) is worth reporting either way.

// netRefFrom builds an ObjectRef from the {namespace}/{name} path values.
func netRefFrom(r *http.Request) kube.ObjectRef {
	return kube.ObjectRef{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
}

// getNetworkOverview returns the cluster's north-south contract and the apps published through it.
func (s *Server) getNetworkOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.app.NetworkOverview(r.Context(), actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

// listServices lists the cluster's Services; ?namespace= scopes to one (absent = all).
func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	svcs, err := s.app.Services(r.Context(), actorFrom(r), r.PathValue("id"), r.URL.Query().Get("namespace"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, svcs)
}

// getService returns one Service's detail (metadata + the endpoints behind it).
func (s *Server) getService(w http.ResponseWriter, r *http.Request) {
	d, err := s.app.Service(r.Context(), actorFrom(r), r.PathValue("id"), netRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// getServiceManifest returns a Service's YAML.
func (s *Server) getServiceManifest(w http.ResponseWriter, r *http.Request) {
	y, err := s.app.ServiceManifest(r.Context(), actorFrom(r), r.PathValue("id"), netRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(y))
}

// getServiceEvents returns a Service's events.
func (s *Server) getServiceEvents(w http.ResponseWriter, r *http.Request) {
	ev, err := s.app.ServiceEvents(r.Context(), actorFrom(r), r.PathValue("id"), netRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// listGateways lists the cluster's Gateway API Gateways (empty without the envoy-gateway add-on).
func (s *Server) listGateways(w http.ResponseWriter, r *http.Request) {
	gws, err := s.app.Gateways(r.Context(), actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, gws)
}

// getGatewayManifest returns a Gateway's YAML.
func (s *Server) getGatewayManifest(w http.ResponseWriter, r *http.Request) {
	y, err := s.app.GatewayManifest(r.Context(), actorFrom(r), r.PathValue("id"), netRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(y))
}

// listRoutes lists the cluster's Gateway API routes; ?namespace= scopes to one (absent = all).
func (s *Server) listRoutes(w http.ResponseWriter, r *http.Request) {
	rs, err := s.app.Routes(r.Context(), actorFrom(r), r.PathValue("id"), r.URL.Query().Get("namespace"))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

// getRouteManifest returns a route's YAML. An unknown {kind} is a 400 - it names a resource type, so
// it is validated here rather than forwarded to kubectl.
func (s *Server) getRouteManifest(w http.ResponseWriter, r *http.Request) {
	kind, ok := kube.ParseRouteKind(r.PathValue("kind"))
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unknown route kind %q", r.PathValue("kind")))
		return
	}
	y, err := s.app.RouteManifest(r.Context(), actorFrom(r), r.PathValue("id"), kind, netRefFrom(r))
	if err != nil {
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(y))
}

// --- Monitoring --------------------------------------------------------------

// getMonitoringTabs returns the Monitoring page's tab descriptors plus whether the cluster is Ready
// and has the monitoring stack installed - so the page renders the not-Ready / not-enabled states
// without issuing a query.
func (s *Server) getMonitoringTabs(w http.ResponseWriter, r *http.Request) {
	c, err := s.app.GetCluster(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, monitoringMeta{
		Enabled:      monitoring.Enabled(c),
		Ready:        c.Phase == domain.PhaseReady,
		Tabs:         s.app.MonitoringTabs(),
		Ranges:       monitoring.Ranges,
		DefaultRange: monitoring.DefaultRange,
		Apps:         s.app.TunnelApps(tunnel.SurfaceMonitoring),
	})
}

type monitoringMeta struct {
	Enabled      bool                 `json:"enabled"`
	Ready        bool                 `json:"ready"`
	Tabs         []monitoring.TabMeta `json:"tabs"`
	Ranges       []string             `json:"ranges"`        // selectable time-range picker windows
	DefaultRange string               `json:"default_range"` // the window the page opens with
	Apps         []tunnel.App         `json:"apps"`          // in-cluster UIs the "Open UI" links point at
}

// getMonitoringTab resolves one tab's panels. Not-Ready → 409, not-enabled → 409, unknown tab → 404,
// cross-tenant → 404 (via workloadStatusFor / statusFor).
func (s *Server) getMonitoringTab(w http.ResponseWriter, r *http.Request) {
	data, err := s.app.Monitoring(r.Context(), actorFrom(r), r.PathValue("id"), r.PathValue("tab"), r.URL.Query().Get("window"))
	if err != nil {
		switch {
		case errors.Is(err, monitoring.ErrUnknownTab):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, app.ErrMonitoringNotEnabled):
			writeErr(w, http.StatusConflict, err)
		default:
			writeErr(w, workloadStatusFor(err), err)
		}
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// tunnelRedirect sends a bare /clusters/{id}/proxy/{app} to its trailing-slash form, the root path
// the in-cluster UIs serve from (a subpath without the slash confuses their relative asset loading).
// The Location is RELATIVE (just the app segment + slash) so the browser resolves it against its own
// /api-prefixed URL; a server-absolute path here would drop nginx's /api prefix and 404 on the SPA.
func (s *Server) tunnelRedirect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Location", path.Base(r.URL.Path)+"/")
	w.WriteHeader(http.StatusMovedPermanently)
}

// tunnel reverse-proxies a browser request to a cluster's in-cluster monitoring UI. The App layer
// does the gating (view-scope, Ready, monitoring-enabled) and, on success, takes over the response
// to stream the proxied bytes - so an error here is a pre-flight failure written the normal way,
// while anything after the proxy starts is handled inside the Proxier.
func (s *Server) tunnel(w http.ResponseWriter, r *http.Request) {
	err := s.app.ServeTunnel(w, r, actorFrom(r), r.PathValue("id"), r.PathValue("app"))
	if err != nil {
		writeErr(w, tunnelStatusFor(err), err)
	}
}

// tunnelStatusFor maps a tunnel pre-flight error to an HTTP status: unknown app → 404, not-Ready and
// not-enabled → 409, otherwise the usual mapping (404 cross-tenant, 401, 500).
func tunnelStatusFor(err error) int {
	switch {
	case errors.Is(err, app.ErrUnknownTunnelApp):
		return http.StatusNotFound
	case errors.Is(err, app.ErrClusterNotReady), errors.Is(err, app.ErrMonitoringNotEnabled):
		return http.StatusConflict
	default:
		return statusFor(err)
	}
}

// --- Security ----------------------------------------------------------------

// getSecurityMeta returns the Security page's report-kind descriptors plus whether the cluster is
// Ready and has the Trivy Operator add-on installed - so the page renders the not-Ready / not-enabled
// states without issuing a query.
func (s *Server) getSecurityMeta(w http.ResponseWriter, r *http.Request) {
	c, err := s.app.GetCluster(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, securityMeta{
		Enabled: security.Enabled(c),
		Ready:   c.Phase == domain.PhaseReady,
		Kinds:   s.app.SecurityKinds(),
	})
}

type securityMeta struct {
	Enabled bool                `json:"enabled"`
	Ready   bool                `json:"ready"`
	Kinds   []security.KindMeta `json:"kinds"`
}

// getSecurityOverview resolves the cluster-wide security dashboard. Not-Ready → 409, not-enabled →
// 409, cross-tenant → 404.
func (s *Server) getSecurityOverview(w http.ResponseWriter, r *http.Request) {
	data, err := s.app.SecurityOverview(r.Context(), actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, securityStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// listSecurityReports lists one kind's report summaries. Unknown kind → 404.
func (s *Server) listSecurityReports(w http.ResponseWriter, r *http.Request) {
	kind, ok := security.ParseKind(r.PathValue("kind"))
	if !ok {
		writeErr(w, http.StatusNotFound, security.ErrUnknownKind)
		return
	}
	data, err := s.app.SecurityReports(r.Context(), actorFrom(r), r.PathValue("id"), kind)
	if err != nil {
		writeErr(w, securityStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// getSecurityReport returns one report's full finding list. The report is identified by kind (path)
// plus name and namespace (query) - namespace is empty for a cluster-scoped ClusterRole assessment,
// which is why it rides in the query string rather than a path segment.
func (s *Server) getSecurityReport(w http.ResponseWriter, r *http.Request) {
	kind, ok := security.ParseKind(r.PathValue("kind"))
	if !ok {
		writeErr(w, http.StatusNotFound, security.ErrUnknownKind)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name query parameter is required"))
		return
	}
	data, err := s.app.SecurityReport(r.Context(), actorFrom(r), r.PathValue("id"), kind, r.URL.Query().Get("namespace"), name)
	if err != nil {
		writeErr(w, securityStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// securityStatusFor maps app/security errors to HTTP status: not-enabled and not-Ready are 409, an
// unknown kind is 404, everything else falls through to the shared mapper (cross-tenant → 404).
func securityStatusFor(err error) int {
	switch {
	case errors.Is(err, app.ErrSecurityNotEnabled):
		return http.StatusConflict
	case errors.Is(err, security.ErrUnknownKind):
		return http.StatusNotFound
	default:
		return workloadStatusFor(err)
	}
}

// --- Audit --------------------------------------------------------------------

// getAuditEvents returns a page of the cluster's API-server audit events. View-scoped; not-Ready → 409,
// cross-tenant → 404 (via the shared mapper). Filters ride in the query string.
func (s *Server) getAuditEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	data, err := s.app.AuditEvents(r.Context(), actorFrom(r), r.PathValue("id"), audit.Query{
		Limit:     limit,
		Verb:      q.Get("verb"),
		Namespace: q.Get("namespace"),
		User:      q.Get("user"),
		Resource:  q.Get("resource"),
		Search:    q.Get("q"),
	})
	if err != nil {
		// not-Ready → 409, everything else via the shared workload mapper (cross-tenant → 404).
		writeErr(w, workloadStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// --- Auth & users ------------------------------------------------------------

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// register self-services a new tenant account (zero quota) and logs it in. 403 when the deployment
// authenticates against a directory - accounts there come from the directory on first login.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.app.Register(req.Username, req.Password)
	if err != nil {
		// Not an unconditional 400: ErrRegistrationDisabled must surface as a 403 so the portal can
		// tell "this deployment doesn't do that" apart from "your input was bad".
		if errors.Is(err, app.ErrRegistrationDisabled) {
			writeErr(w, http.StatusForbidden, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err) // taken username, weak password, invalid name
		return
	}
	s.setSessionCookie(w, r, u.ID)
	writeJSON(w, http.StatusCreated, u)
}

// login verifies credentials and sets the session cookie. Public and unauthenticated, which is why
// the app layer throttles it (see internal/app/throttle.go) - clientIP is for that alone.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.app.Login(r.Context(), req.Username, req.Password, clientIP(r))
	if err != nil {
		// statusFor maps the throttle to 429 and bad credentials to 401. A directory that is down
		// is a 500, deliberately: it is not the user's password that is wrong, and telling them it
		// is would send them to reset a password that works fine.
		writeErr(w, statusFor(err), err)
		return
	}
	s.setSessionCookie(w, r, u.ID)
	writeJSON(w, http.StatusOK, u)
}

// authConfig reports the deployment's authentication shape to the login page. Public - it is read
// before there is any session to read it with.
func (s *Server) authConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.app.AuthConfig())
}

// logout clears the session cookie (idempotent; public).
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// me returns the currently authenticated user (the SPA uses it to gate the app and show admin UI).
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, actorFrom(r))
}

// profile returns the actor's own account view: identity, their groups with the role held in each,
// and their per-infrastructure quota. Self-scoped, so any authenticated caller may ask.
func (s *Server) profile(w http.ResponseWriter, r *http.Request) {
	rep, err := s.app.Profile(actorFrom(r))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// listUsers returns every account with usage plus the platform allocation summary. Admin only.
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	rep, err := s.app.ListUsers(actorFrom(r))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// updateUser applies a quota grant, a group assignment, or both, to an account. Admin only.
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var req app.UpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.app.UpdateUser(actorFrom(r), r.PathValue("id"), req)
	if err != nil {
		// Allocation-invariant violations are neither NotFound nor Forbidden - surface as 400.
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// deleteUser removes an account and cascades teardown of its clusters. Admin only.
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	err := s.app.DeleteUser(actorFrom(r), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err) // self-delete or last-admin guard
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Groups ------------------------------------------------------------------

// createGroup creates an admin-managed team. Admin only.
func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.app.CreateGroup(actorFrom(r), req.Name)
	if err != nil {
		if errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

// listGroups returns every group with its members' usernames. Admin only.
func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.app.ListGroups(actorFrom(r))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

// renameGroup renames an existing group. Admin only.
func (s *Server) renameGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.app.RenameGroup(actorFrom(r), r.PathValue("id"), req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// deleteGroup removes a group and ungroups its members; their clusters are untouched. Admin only.
func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteGroup(actorFrom(r), r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, app.ErrRegistrationDisabled):
		return http.StatusForbidden
	case errors.Is(err, app.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, app.ErrInvalidCredentials):
		return http.StatusUnauthorized
	case errors.Is(err, app.ErrTooManyAttempts):
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
