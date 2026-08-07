// Package registry is the seam behind the platform's container image registry: it provisions each
// cluster's own project and pull credential, converges the per-user/-group project memberships that
// mirror the portal's read/write model, and serves the Registry page's read-only queries.
//
// There is ONE central registry (Harbor), always optional, deployed next to the platform (a compose
// overlay and a dependency of the deploy/helm/kaas chart). "Per-cluster" is only a PROJECT and a
// robot account - never a registry per cluster. The layout is:
//
//	<prefix>library                platform-owned images (admins write, every cluster may pull)
//	<prefix>cache-<upstream>       pull-through proxy caches (docker.io, ghcr.io, quay.io, k8s)
//	<prefix><cluster name>         one private project per cluster, the tenant's own images
//
// This is deliberately the same shape as internal/vault, because it is the same problem: a central
// service that knows nothing about the platform's ownership/group model, kept converged with
// Postgres by the platform as its single writer. DesiredAccess below is the pure mapping,
// SyncAccess applies it, and the SAME function decides what the portal's Registry page shows - so
// what a user can see and what the registry lets them do cannot drift.
//
// Two responsibilities, split the way the reconcile loop already splits per-cluster from singleton
// work (and exactly as the Vault integration splits them):
//
//   - Per-cluster lifecycle (EnsureCluster/ReleaseCluster) is reconcile-loop work, gated by
//     Cluster.RegistryWired. ReleaseCluster runs BEFORE the infrastructure is destroyed.
//   - Access convergence (SyncAccess) runs under the leader lease on a ticker, because membership
//     edits happen API-side and never bump a cluster's generation.
//
// THE THIRD HALF IS NOT HERE. A node has to trust the registry and know its mirrors before its
// FIRST image pull, which happens during bring-up, long before a cluster is Ready - so node trust is
// not reconcile-loop work at all. It rides in the Ansible bootstrap path (the registry_trust role,
// driven by NodeTrust below), and it deliberately carries NO credential. See NodeTrust.
package registry

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// DefaultProjectPrefix keeps every project the platform owns clear of anything else in a registry
// that is already used for other work - the same rationale as the "kaas-" prefix on Vault's policies
// and identity groups.
const DefaultProjectPrefix = "kaas-"

// Auth modes the platform configures in the registry, chosen to follow the portal's own KAAS_AUTH:
// local → Harbor's local database (db_auth), ldap → the same directory the portal authenticates
// against (ldap_auth).
const (
	AuthLocal = "local"
	AuthLDAP  = "ldap"
)

// LibraryProject is the platform's own project: images the operator publishes for every cluster.
const LibraryProject = "library"

// DefaultUpstreams are the registries the platform proxies by default - the four every add-on in the
// catalog pulls from. Each becomes one Harbor proxy-cache project plus one containerd hosts.toml
// entry on every node.
//
// Type must be one of the ADAPTERS THE REGISTRY ITSELF DECLARES, not the obvious name for the
// service: Harbor answers `GET /api/v2.0/replication/adapters` with the list, an unknown value comes
// back as an opaque `500 internal server error`, and the two that are easy to get wrong are here -
// ghcr.io is `github-ghcr` (not `github`), and quay.io has NO adapter of its own, so it is proxied as
// a plain `docker-registry`. Verified against Harbor v2.15.
var DefaultUpstreams = []Upstream{
	{Name: "dockerhub", Host: "docker.io", Endpoint: "https://hub.docker.com", Type: "docker-hub"},
	{Name: "ghcr", Host: "ghcr.io", Endpoint: "https://ghcr.io", Type: "github-ghcr"},
	{Name: "quay", Host: "quay.io", Endpoint: "https://quay.io", Type: "docker-registry"},
	{Name: "k8s", Host: "registry.k8s.io", Endpoint: "https://registry.k8s.io", Type: "docker-registry"},
}

// Upstream is one proxied registry: the host a node writes in an image reference, the endpoint the
// registry dials, and the provider type the registry's own remote-registry API wants.
type Upstream struct {
	Name     string // short key, used in the cache project name
	Host     string // the image-reference host nodes mirror ("docker.io")
	Endpoint string // what the registry dials upstream
	Type     string // the registry backend's provider type
}

// Settings is the deployment-level registry configuration.
//
// URL and Addr are SPLIT ON PURPOSE, the same lesson as KAAS_VAULT_ADDR vs KAAS_VAULT_CLUSTER_ADDR -
// and it bites harder here, because the cluster-facing name is baked into every image reference and
// into the certificate's SANs. URL is the platform's own route to the registry API; Host is the
// host:port a cluster NODE puts in an image reference and in its containerd hosts.toml. Collapsing
// them yields "x509: certificate is valid for ..." on every node of every cluster, at bring-up.
type Settings struct {
	// URL is the registry API base the API and worker reach, e.g. "https://harbor:8443".
	URL string
	// Host is the image-reference host cluster nodes use, e.g. "harbor.lab:443". Defaults to URL's
	// host when unset, which is right whenever both sides share one route (up-fake, a routable host).
	Host string
	// UIURL is the browser-facing base of the registry UI, used to build the portal's "Open Harbor"
	// link - never used to reach the API. Defaults to URL.
	UIURL string
	// Username/Password authenticate API calls: the ADMIN credential on the worker, or the narrow
	// read-only portal robot on the API. Never a Helm value inside a cluster.
	Username string
	Password string
	// ProjectPrefix namespaces every project the platform owns (DefaultProjectPrefix when empty).
	ProjectPrefix string
	// AuthMode is the registry auth backend to configure (AuthLocal|AuthLDAP), mirroring KAAS_AUTH.
	AuthMode string
	// LDAP carries the directory settings when AuthMode==AuthLDAP, translated from the portal's own
	// authn/ldap config so the registry authenticates users against the same directory with the same
	// login attribute. Nil in local mode.
	LDAP *LDAPAuth
	// ManageAuth is whether this process may write the registry's auth configuration at all.
	//
	// It exists because the WORKER - the process that runs EnsurePlatform - deliberately gets neither
	// KAAS_AUTH nor the mounted ldap.yaml: the directory bind password is kept out of the container
	// holding the libvirt socket and every tenant's secrets. So on a directory-authenticated
	// deployment the worker's own view of KAAS_AUTH is "local", and writing that to the registry
	// would flip a correctly-configured ldap_auth back to db_auth and lock every user out.
	//
	// False therefore means "I do not know enough to have an opinion" - the registry's identity
	// configuration is left exactly as the operator set it, and everything else (projects, robots,
	// memberships) still converges. It is false in two cases: KAAS_AUTH was never set in this process
	// (the worker's normal state), or a directory was requested but its configuration could not be
	// read here. Standing down costs nothing on a genuinely local deployment - a registry's own
	// default is already its user database.
	ManageAuth bool
	// Mirror turns the pull-through cache on: the platform creates a proxy-cache project per
	// upstream and every node's containerd is pointed at them. Off, the registry is a tenant
	// registry only and nothing about cluster bring-up changes. The two halves are independent by
	// construction, so this can be turned off without giving up per-cluster projects.
	Mirror bool
	// Upstreams are the registries proxied when Mirror is on (DefaultUpstreams when empty).
	Upstreams []Upstream
	// UpstreamAuth optionally authenticates the proxy cache to an upstream, keyed by Upstream.Name.
	// Docker Hub is the one that matters: an anonymous proxy cache inherits Docker Hub's anonymous
	// pull rate limit for the WHOLE fleet, which is the failure this integration is partly meant to
	// remove.
	UpstreamAuth map[string]UpstreamCredential
	// CAFile is the PEM the platform hands cluster nodes so they trust the registry's certificate.
	// Empty means the certificate already chains to a public root (or Insecure is set).
	CAFile string
	// Insecure skips TLS verification, platform-side AND on cluster nodes. A documented lab
	// shortcut for a self-signed registry that has not been given a CA file.
	Insecure bool
	// RetainProject keeps a deleted cluster's project (and its images) instead of releasing it.
	// Off by default: a project is cluster-scoped state and the platform releases it with the
	// cluster, exactly as it releases the cluster's Vault path and its DNS record.
	RetainProject bool
	// RobotTTL bounds a minted robot credential's life. Recorded per cluster as
	// Cluster.RegistryRobotNotAfter so a later rotation sweep has a due-date to read.
	RobotTTL time.Duration
	// ProjectQuotaGB caps a cluster project's storage; 0 = unlimited.
	ProjectQuotaGB int
}

// UpstreamCredential authenticates the proxy cache to one upstream registry.
type UpstreamCredential struct {
	Username string
	Password string
}

// LDAPAuth is the subset of directory settings the registry's ldap auth needs, translated from the
// portal's authn/ldap.Config by the caller (app.buildRegistryManager) so this package stays
// decoupled from the authn package - the same translation vault.LDAPAuth gets.
type LDAPAuth struct {
	URL          string // ldaps://dc.example.lab
	BindDN       string
	BindPassword string
	BaseDN       string // user_base_dn
	UID          string // the login attribute (sAMAccountName)
	VerifyCert   bool
}

// Enabled reports whether a real registry is configured (an address is set). The Fake is used
// otherwise, so the whole flow stays demoable with no registry at all.
func (s Settings) Enabled() bool { return strings.TrimSpace(s.URL) != "" }

// CanSetPasswords reports whether THIS process's credential can generate a registry password for a
// user - which needs local auth mode (in directory mode the directory owns the credential and a
// second, divergent password would be a liability rather than a convenience) AND an admin account
// rather than a robot.
//
// The robot check is not cosmetic: a robot cannot set a user's password, so without it the portal
// would advertise a button that fails at the moment a user presses it. The API is normally given the
// read-only portal robot, so the self-service password button is OFF unless an operator has
// deliberately handed the API an admin credential - which the configuration reference says out loud,
// because it is a real widening of what a compromised API replica can do.
func (s Settings) CanSetPasswords() bool {
	return s.AuthMode == AuthLocal && !strings.HasPrefix(s.Username, "robot$")
}

// prefix returns the project prefix, defaulting to DefaultProjectPrefix.
func (s Settings) prefix() string {
	if strings.TrimSpace(s.ProjectPrefix) == "" {
		return DefaultProjectPrefix
	}
	return strings.TrimSpace(s.ProjectPrefix)
}

// Validate normalizes the settings and defaults, and refuses a configuration that cannot work.
// Called at startup so a half-configured registry fails to boot rather than failing every cluster's
// bring-up at the moment a node tries its first pull.
func (s Settings) Validate() (Settings, error) {
	s.URL = strings.TrimRight(strings.TrimSpace(s.URL), "/")
	s.ProjectPrefix = s.prefix()
	if s.UIURL == "" {
		s.UIURL = s.URL
	}
	s.UIURL = strings.TrimRight(s.UIURL, "/")
	if strings.TrimSpace(s.Host) == "" {
		s.Host = hostOf(s.URL)
	}
	s.Host = strings.TrimSpace(s.Host)
	switch s.AuthMode {
	case "", AuthLocal:
		s.AuthMode = AuthLocal
	case AuthLDAP:
		// The directory settings are needed to WRITE the registry's auth configuration, not to know
		// which mode this deployment is in - so they are required only where ManageAuth says this
		// process is the one that writes it. The distinction is load-bearing for the worker, which is
		// given the mode but deliberately not the bind password: requiring them here forced it to
		// call itself "local" instead, and AuthMode==local is what makes SyncAccess create a LOCAL
		// account per user. In a directory deployment that both shadows every directory user and -
		// because a registry refuses to change auth mode once its database holds users - permanently
		// locks the registry into the wrong mode.
		if s.ManageAuth && (s.LDAP == nil || strings.TrimSpace(s.LDAP.URL) == "") {
			return s, fmt.Errorf("registry: AuthMode=ldap needs LDAP settings to configure the registry with")
		}
	default:
		return s, fmt.Errorf("registry: unknown AuthMode %q (want local|ldap)", s.AuthMode)
	}
	if len(s.Upstreams) == 0 {
		s.Upstreams = DefaultUpstreams
	}
	if s.RobotTTL <= 0 {
		s.RobotTTL = 365 * 24 * time.Hour
	}
	if !validProjectPrefix(s.ProjectPrefix) {
		return s, fmt.Errorf("registry: project prefix %q must be lowercase alphanumeric with -._ separators", s.ProjectPrefix)
	}
	return s, nil
}

// validProjectPrefix mirrors the registry's own project-name rule (lowercase alphanumerics and
// -._ separators). A prefix that fails it makes EVERY project creation fail at reconcile time, one
// cluster at a time - so it is caught at start-up instead.
func validProjectPrefix(p string) bool {
	if p == "" {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// nodeTrust assembles what a cluster node needs from these settings plus the CA. One place, so
// nothing can render the mirror list two different ways.
func (s Settings) nodeTrust(caPEM string) NodeTrust {
	t := NodeTrust{Host: s.Host, CAPEM: caPEM, Insecure: s.Insecure}
	if !s.Mirror {
		return t
	}
	for _, u := range s.Upstreams {
		t.Mirrors = append(t.Mirrors, NodeMirror{
			Host:   u.Host,
			Server: "https://" + u.Host,
			Mirror: s.URLForCluster() + "/v2/" + s.CacheProject(u),
		})
	}
	return t
}

// URLForCluster is the registry base a cluster NODE dials - built from Host, not URL. See the
// Settings doc: the platform's own route and the node-facing one are not the same address.
func (s Settings) URLForCluster() string {
	if s.Insecure {
		return "http://" + s.Host
	}
	return "https://" + s.Host
}

// NodeTrustFor assembles the node trust config from these settings and the CA PEM. Exported because
// EVERY process derives it independently - it needs no registry round-trip and no minted secret, so
// a worker replica can configure a node's containerd whether or not it is the leader.
func (s Settings) NodeTrustFor(caPEM string) NodeTrust {
	return s.nodeTrust(caPEM)
}

// hostOf strips the scheme (and any path) off a URL, leaving the host:port an image reference wants.
func hostOf(url string) string {
	h := url
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.Index(h, "/"); i >= 0 {
		h = h[:i]
	}
	return h
}

// --- naming ---------------------------------------------------------------------------------
//
// Every project the platform owns is named deterministically, so the mapping is re-derivable and
// every write is an idempotent upsert.
//
// A cluster's project is keyed on its NAME, not its id - unlike Vault's KV path. The difference is
// deliberate: this string appears in every image reference a user ever types
// ("harbor.lab/kaas-dev/api:1"), where a UUID would be hostile. Cluster names are already globally
// unique (which is why the DNS integration needs no allocator either), and a name is immutable once
// created, so the key is as stable as an id in practice.

// ClusterProject is a cluster's own private project.
func (s Settings) ClusterProject(c *domain.Cluster) string { return s.prefix() + c.Name }

// CacheProject is the pull-through proxy project for one upstream.
func (s Settings) CacheProject(u Upstream) string { return s.prefix() + "cache-" + u.Name }

// Library is the platform's own public project.
func (s Settings) Library() string { return s.prefix() + LibraryProject }

// ClusterRobot names a cluster's push+pull robot (the registry prepends its own "robot$").
func ClusterRobot(c *domain.Cluster) string { return "kaas-cluster-" + c.ID }

// UIProjectPath is the registry UI deep-link for a project - where the portal's per-cluster
// "Open in Harbor" lands.
// It returns EMPTY rather than a relative path when no UI address is configured. A caller that
// renders the result as a link would otherwise produce one pointing back at the portal itself - the
// browser resolves a relative href against the current page - which reads as a broken link to the
// registry rather than as the missing configuration it actually is.
func (s Settings) UIProjectPath(project string) string {
	if strings.TrimSpace(s.UIURL) == "" {
		return ""
	}
	return fmt.Sprintf("%s/harbor/projects?project_name=%s", strings.TrimRight(s.UIURL, "/"), project)
}

// PullReference is the image-reference prefix a user pushes to for this cluster, e.g.
// "harbor.lab/kaas-dev". Rendered by the portal as the push instructions.
func (s Settings) PullReference(c *domain.Cluster) string {
	return s.Host + "/" + s.ClusterProject(c)
}

// --- access model ---------------------------------------------------------------------------

// Role is a project membership level, in the registry's own vocabulary. The platform only ever uses
// these three: everything else Harbor offers has no counterpart in the portal's model.
type Role string

const (
	RoleProjectAdmin Role = "projectAdmin" // the cluster's owner
	RoleDeveloper    Role = "developer"    // pull + push: a write-role group-mate
	RoleGuest        Role = "guest"        // pull only: a read-role group-mate
)

// AccessSnapshot is the whole of the platform's authorization state, as read from the store by the
// leader-elected SyncAccess sweep. DesiredAccess turns it into the projects and memberships that
// mirror it.
type AccessSnapshot struct {
	Users    []*domain.User
	Groups   []*domain.Group
	Clusters []*domain.Cluster // live clusters only (the caller filters out deleted ones)
}

// DesiredMember is one project membership: a platform user at a role on one project.
type DesiredMember struct {
	Project  string
	Username string
	Role     Role
}

// DesiredUser is one registry account the platform expects to exist. In LDAP mode the directory
// owns the account and only SysAdmin is applied; in local mode the platform creates it.
type DesiredUser struct {
	Username string
	Email    string
	Realname string
	SysAdmin bool // platform admins are registry system admins - they already see every cluster
}

// DesiredState is the full set of registry objects SyncAccess converges to.
type DesiredState struct {
	Users   []DesiredUser
	Members []DesiredMember
}

// DesiredAccess computes the registry memberships that mirror the platform's access model. It is a
// pure function of the snapshot - no registry, no I/O - so the whole "who may pull/push which
// cluster's project" decision is unit-tested without a registry, exactly like vault.DesiredAccess
// and domain.EtcdDefragPolicy.
//
// The mapping is a direct transcription of app.accessTo:
//   - owner → projectAdmin on their own clusters' projects.
//   - platform admin → a registry SYSTEM admin (so no per-project rows are needed for them at all,
//     which also keeps the membership set from growing by O(admins x clusters)).
//   - group-mate → for a cluster X whose owner is in group G, every member of G gets membership on
//     X's project at THEIR role in G: write → developer (pull+push), read → guest (pull only).
//
// A user in several groups that all reach one cluster gets the HIGHEST role, matching accessTo.
//
// MEMBERSHIP IS PER USER, NEVER PER DIRECTORY GROUP. Harbor can take an LDAP group DN as a member and
// it is tempting in ldap mode, but this platform's directory mapping is a list of arbitrary LDAP
// FILTERS (several of which may share one group_key) and a platform group can mix directory-derived
// and locally-added members - so there is frequently no single group DN that means "this platform
// group". Converging per user is one mechanism for both auth modes and keeps Postgres the single
// source of truth.
func DesiredAccess(snap AccessSnapshot, s Settings) DesiredState {
	usersByID := make(map[string]*domain.User, len(snap.Users))
	for _, u := range snap.Users {
		usersByID[u.ID] = u
	}

	var users []DesiredUser
	for _, u := range snap.Users {
		users = append(users, DesiredUser{
			Username: strings.ToLower(u.Username),
			Email:    userEmail(u),
			Realname: u.Username,
			SysAdmin: u.IsAdmin,
		})
	}

	// project -> username -> role, so the highest role wins per (project, user) pair.
	best := map[string]map[string]Role{}
	put := func(project, username string, role Role) {
		if project == "" || username == "" {
			return
		}
		if best[project] == nil {
			best[project] = map[string]Role{}
		}
		if rank(role) > rank(best[project][username]) {
			best[project][username] = role
		}
	}

	for _, c := range snap.Clusters {
		project := s.ClusterProject(c)
		owner := usersByID[c.OwnerID]
		if owner == nil {
			continue
		}
		put(project, strings.ToLower(owner.Username), RoleProjectAdmin)
		// The clusters a group can reach are exactly those whose OWNER is in the group - the same
		// derivation accessTo and vault.DesiredAccess make.
		for _, u := range snap.Users {
			if u.ID == owner.ID || u.IsAdmin {
				continue // the owner is already projectAdmin; admins are system admins
			}
			role, ok := groupRoleBetween(u, owner)
			if !ok {
				continue
			}
			if role == domain.GroupRoleWrite {
				put(project, strings.ToLower(u.Username), RoleDeveloper)
			} else {
				put(project, strings.ToLower(u.Username), RoleGuest)
			}
		}
	}

	var members []DesiredMember
	for project, byUser := range best {
		for username, role := range byUser {
			members = append(members, DesiredMember{Project: project, Username: username, Role: role})
		}
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Project != members[j].Project {
			return members[i].Project < members[j].Project
		}
		return members[i].Username < members[j].Username
	})
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	return DesiredState{Users: users, Members: members}
}

// ProjectsForUser is the set of project names a user may see right now, across every cluster they
// can reach, with the role they hold on each. It is what the portal's Registry page filters by, so
// the page and the registry's own enforcement are computed by ONE function and cannot drift. A
// platform admin sees everything (nil = "no filter").
func ProjectsForUser(actor *domain.User, clusters []*domain.Cluster, owners map[string]*domain.User, s Settings) map[string]Role {
	if actor == nil {
		return map[string]Role{}
	}
	if actor.IsAdmin {
		return nil
	}
	out := map[string]Role{}
	for _, c := range clusters {
		project := s.ClusterProject(c)
		if c.OwnerID == actor.ID {
			out[project] = RoleProjectAdmin
			continue
		}
		owner := owners[c.OwnerID]
		if owner == nil {
			continue
		}
		role, ok := groupRoleBetween(actor, owner)
		if !ok {
			continue
		}
		want := RoleGuest
		if role == domain.GroupRoleWrite {
			want = RoleDeveloper
		}
		if rank(want) > rank(out[project]) {
			out[project] = want
		}
	}
	// The library and the caches are readable by everyone: they hold the platform's own and the
	// upstream public images, which every cluster already pulls.
	out[s.Library()] = RoleGuest
	for _, u := range s.Upstreams {
		out[s.CacheProject(u)] = RoleGuest
	}
	return out
}

// groupRoleBetween returns the highest role `actor` holds in any group `owner` also belongs to.
func groupRoleBetween(actor, owner *domain.User) (domain.GroupRole, bool) {
	found := false
	best := domain.GroupRole("")
	for _, m := range actor.Memberships {
		if !owner.InGroup(m.GroupID) {
			continue
		}
		found = true
		if m.Role == domain.GroupRoleWrite {
			return domain.GroupRoleWrite, true
		}
		best = domain.GroupRoleRead
	}
	return best, found
}

func rank(r Role) int {
	switch r {
	case RoleProjectAdmin:
		return 3
	case RoleDeveloper:
		return 2
	case RoleGuest:
		return 1
	}
	return 0
}

// RandomToken returns n bytes of cryptographic randomness, base64url-encoded without padding - the
// alphabet every registry accepts in a password. It backs both the throwaway password a local
// account is created with and the one the portal generates for its owner, so there is one source of
// randomness rather than a hand-rolled one per call site.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := cryptorand.Read(b); err != nil {
		// crypto/rand does not fail on any supported platform; if it ever does, a predictable
		// credential is far worse than a panic at the moment of minting.
		panic("registry: no randomness available: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// GeneratePassword mints a registry password that satisfies the usual complexity rule (8+
// characters with an upper, a lower and a digit) while still being mostly random. This is what the
// portal's "Generate registry password" hands its owner, once - the platform stores nothing.
func GeneratePassword() string {
	return "Kh" + RandomToken(18) + "9z"
}

// userEmail synthesizes the address the registry insists every account carries. Harbor requires a
// unique, syntactically valid email per user and the platform's own accounts have none, so this is a
// deterministic placeholder rather than a claim about a real mailbox.
func userEmail(u *domain.User) string {
	return strings.ToLower(u.Username) + "@kaas.local"
}

// --- node trust ------------------------------------------------------------------------------

// NodeTrust is everything a cluster NODE needs to pull through the registry: the CA to trust, the
// host to reach, and - when the mirror is on - one containerd hosts.toml mirror per upstream.
//
// This is handed to Ansible on the BOOTSTRAP path (every playbook, via the config manager's shared
// extra-vars), not applied by the reconcile loop: a node's first image pull happens during bring-up,
// long before the cluster is Ready, so trust that arrived at Ready would arrive too late to be the
// point. A zero NodeTrust means "this deployment has no registry", and the role is a hard no-op.
//
// IT CARRIES NO CREDENTIAL, and that is load-bearing rather than incidental. The obvious design - a
// fleet-wide pull-only robot in every node's hosts.toml - fails three ways at once: containerd 2.x
// removed static registry auth from config.toml so the secret would have to live in a Basic
// authorization header in a file on every cluster VM; a robot's secret is returned only at creation,
// so replicas that each re-minted it would invalidate each other's copy; and it puts a standing
// fleet credential on every tenant's node for no gain. The proxy caches are PUBLIC instead
// (anonymous pull), which is honest about what they hold: public upstream images that anyone reaching
// the registry could pull from the upstream anyway. Private images are a different mechanism
// entirely - the cluster's OWN project, reached with the per-cluster robot delivered as an ordinary
// Kubernetes imagePullSecret.
//
// The second benefit is structural: with no minted secret in it, NodeTrust is a pure function of the
// settings plus the CA file, so EVERY replica derives the same value with no coordination - nothing
// is pinned to whichever replica happened to run the platform setup.
type NodeTrust struct {
	Host     string       `json:"host"`
	CAPEM    string       `json:"ca_pem,omitempty"`
	Insecure bool         `json:"insecure,omitempty"`
	Mirrors  []NodeMirror `json:"mirrors,omitempty"`
}

// Configured reports whether there is anything for the node role to do.
func (t NodeTrust) Configured() bool { return t.Host != "" }

// NodeMirror is one upstream's containerd mirror entry.
type NodeMirror struct {
	// Host is the upstream being mirrored ("docker.io") - the directory name under
	// /etc/containerd/certs.d.
	Host string `json:"host"`
	// Server is the upstream containerd falls back to when the mirror does not answer. This is what
	// makes a registry outage degrade pull SPEED rather than cluster bring-up.
	Server string `json:"server"`
	// Mirror is the full proxy-cache URL, including the project path - which is why the generated
	// hosts.toml must set override_path.
	Mirror string `json:"mirror"`
}

// RobotCredential is a minted robot account: a username the registry recognises and its secret.
type RobotCredential struct {
	Username string    `json:"username"`
	Secret   string    `json:"secret"`
	Expires  time.Time `json:"expires,omitempty"`
}

// Valid reports whether the credential is usable.
func (r RobotCredential) Valid() bool { return r.Username != "" && r.Secret != "" }

// --- query types -----------------------------------------------------------------------------

// Status is the registry's own health, as rendered at the top of the portal's Registry page.
type Status struct {
	Configured bool   `json:"configured"` // false = this deployment runs no registry at all
	Healthy    bool   `json:"healthy"`
	Version    string `json:"version,omitempty"`
	Host       string `json:"host"`   // the image-reference host
	UIURL      string `json:"ui_url"` // "Open Harbor"
	// AuthMode is the mode the REGISTRY IS ACTUALLY IN, read back from it - not the AuthMode this
	// deployment is configured for. The two diverge legitimately and often: the platform only writes
	// the registry's identity configuration from a process that was told how this deployment
	// authenticates (Settings.ManageAuth), so a registry pointed at a fresh Harbor sits on Harbor's
	// own default until someone changes it. Reporting the intent here would tell an operator the
	// directory was wired up when nothing had wired it, which is the one thing this field is read to
	// find out. Falls back to the configured value when the registry will not say (a read-only robot
	// cannot read /configurations).
	AuthMode  string   `json:"auth_mode"`
	Mirror    bool     `json:"mirror"` // is the pull-through cache on?
	Upstreams []string `json:"upstreams,omitempty"`
	// CanSetPassword is true only in local auth mode: the portal offers "Generate registry
	// password" there, and hides it when the directory owns the account.
	CanSetPassword bool   `json:"can_set_password"`
	Message        string `json:"message,omitempty"` // why it is unhealthy, when it is
}

// ProjectKind classifies a project for the portal, which renders each differently.
type ProjectKind string

const (
	KindCluster  ProjectKind = "cluster"  // one cluster's own images
	KindCache    ProjectKind = "cache"    // a pull-through proxy of an upstream
	KindLibrary  ProjectKind = "library"  // the platform's own
	KindExternal ProjectKind = "external" // something else in this registry - shown to admins only
)

// Project is one registry project as the portal shows it.
type Project struct {
	Name       string      `json:"name"`
	Kind       ProjectKind `json:"kind"`
	Public     bool        `json:"public"`
	RepoCount  int         `json:"repo_count"`
	SizeBytes  int64       `json:"size_bytes"`
	QuotaBytes int64       `json:"quota_bytes,omitempty"` // 0 = unlimited
	UpdatedAt  time.Time   `json:"updated_at,omitempty"`
	// ClusterID/ClusterName are set on a cluster project, so the portal can link to the cluster.
	// Resolved by the app layer from the platform's own state, not by the registry.
	ClusterID   string `json:"cluster_id,omitempty"`
	ClusterName string `json:"cluster_name,omitempty"`
	// Role is the ACTOR's role on this project, resolved from the platform's model.
	Role Role `json:"role,omitempty"`
	// Upstream is the proxied registry host, on a cache project.
	Upstream string `json:"upstream,omitempty"`
}

// Repository is one repository within a project.
type Repository struct {
	Name          string    `json:"name"` // without the project prefix
	FullName      string    `json:"full_name"`
	ArtifactCount int       `json:"artifact_count"`
	PullCount     int64     `json:"pull_count"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

// Artifact is one pushed image (or index).
//
// It deliberately carries NO vulnerability summary, even though the registry keeps one: every
// cluster runs the trivy-operator add-on, and internal/security is the single place the platform
// answers "what is wrong with these images" - from what is actually deployed rather than what the
// registry scanned at push time. Harbor still scans on push and blocks the worst findings from
// being pulled (see harbor.EnsureCluster); that stays an enforcement control, not a second report.
type Artifact struct {
	Digest    string    `json:"digest"`
	Tags      []string  `json:"tags,omitempty"`
	SizeBytes int64     `json:"size_bytes"`
	PushedAt  time.Time `json:"pushed_at,omitempty"`
	Type      string    `json:"type,omitempty"` // IMAGE, CHART, ...
}

// --- the seams -------------------------------------------------------------------------------

// Manager is the provisioning seam, held by the WORKER (which carries the registry's admin
// credential). Every write MUST be idempotent - EnsureCluster is level-triggered and re-run on
// retry, ReleaseCluster runs on every deleting tick, SyncAccess re-runs on every sweep.
type Manager interface {
	// EnsurePlatform provisions the platform-wide objects: the auth backend matching the portal, the
	// library project, the (public) proxy-cache projects and their upstream registries, and the
	// garbage-collection schedule that keeps the cache from growing without bound. It provisions no
	// credential: node trust carries none (see NodeTrust).
	EnsurePlatform(ctx context.Context) error
	// EnsureAuth points the registry at the same identity source the portal uses, and is SEPARATE
	// from EnsurePlatform because a different process has to run it. Writing it needs the directory
	// settings, and those reach the API only - the worker deliberately never gets the bind password,
	// which is why it holds the libvirt socket and every tenant's secrets safely. But EnsurePlatform
	// is leader-elected work on the WORKER, so folding auth into it means nothing ever configures it:
	// the registry sits on its own default and every user is told to fix it by hand. Idempotent, and
	// a hard no-op unless Settings.ManageAuth says this process was told how the deployment
	// authenticates.
	EnsureAuth(ctx context.Context) error
	// EnsureCluster provisions a cluster's private project and its push+pull robot. Idempotent: it
	// returns the EXISTING robot's identity with an empty secret when one is already minted, so a
	// re-run neither rotates the credential nor invalidates the cluster's pull secret.
	EnsureCluster(ctx context.Context, c *domain.Cluster) (RobotCredential, error)
	// ReleaseCluster removes a cluster's project and robot. Absence is success.
	ReleaseCluster(ctx context.Context, c *domain.Cluster) error
	// SyncAccess converges accounts and project memberships to DesiredAccess(snap).
	SyncAccess(ctx context.Context, snap AccessSnapshot) error
	// SetUserPassword sets one user's registry password (local auth mode only). The platform never
	// stores the result - it is generated, applied, and shown to its owner once.
	SetUserPassword(ctx context.Context, username, password string) error
}

// Querier is the read seam, held by the API (which carries a narrow read-only robot). Reads only:
// nothing here can change the registry.
type Querier interface {
	Status(ctx context.Context) Status
	Projects(ctx context.Context) ([]Project, error)
	Repositories(ctx context.Context, project string) ([]Repository, error)
	Artifacts(ctx context.Context, project, repo string) ([]Artifact, error)
}
