// Package vault is the seam behind the platform's HashiCorp Vault integration: it provisions each
// cluster's KV subtree and policies, converges the per-user/-group access bindings that mirror the
// portal's own read/write model, and mints the short-lived tokens the "View in Vault" handoff uses.
//
// There is ONE central Vault, always present, deployed next to the platform (a compose service and a
// dependency of the deploy/helm/kaas chart). "Per-cluster" is only a KV *path* and a set of
// policies/roles - never a Vault per cluster. The layout under the KV v2 mount (default "kaas") is:
//
//	kaas/platform/*                       platform-owned secrets (admins only)
//	kaas/clusters/<cluster-id>/<ns>/<n>   per-cluster, per-namespace tenant secrets
//
// The tenant secrets are consumed inside the cluster by the External Secrets Operator, which reads
// only its own cluster's subtree (a per-cluster JWT auth role bound to the read policy - see
// hcvault and the external_secrets ansible role).
//
// AUTHORIZATION MIRRORS THE PLATFORM. Vault does not know the platform's ownership/group model, so
// the platform is the single writer of Vault's policies, identity groups and entities and keeps them
// converged with Postgres (DesiredAccess below is the pure mapping, SyncAccess applies it). This is
// what makes "only users with access to a cluster can touch that cluster's Vault path, writers can
// edit, readers can only view" true in Vault itself rather than only in the portal.
//
// Two responsibilities, split the same way the reconcile loop already splits per-cluster from
// singleton work:
//
//   - Per-cluster lifecycle (EnsureCluster/ReleaseCluster) is driven by the reconcile loop and gated
//     by Cluster.VaultWired, exactly like the DNS registrar (reconcileDNSWiring/releaseDNS). Release
//     runs BEFORE the infrastructure is destroyed.
//   - Access convergence (SyncAccess) runs under the leader lease on a ticker (like GC/metrics),
//     because membership edits happen API-side and never bump a cluster's generation.
//
// The real implementation (internal/vault/hcvault) is a thin net/http client against Vault's REST
// API (the same shape as internal/netbox), built only by components that hold a Vault token - the
// worker for management, the API for the narrow minter used by MintUserToken. The Fake records state
// in-memory and logs, so the whole flow (admission, wiring, the portal page, the handoff) is
// demoable under `make up-fake` with no Vault at all.
package vault

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// DefaultMount is the KV v2 mount the platform owns. Everything the platform writes lives under it,
// so a deployment that already runs Vault for other things keeps its own mounts untouched.
const DefaultMount = "kaas"

// Auth modes the platform configures in Vault, chosen to follow the portal's own auth (KAAS_AUTH):
// local → Vault userpass, ldap → Vault ldap (configured from the same directory settings).
const (
	AuthLocal = "local"
	AuthLDAP  = "ldap"
)

// Settings is the deployment-level Vault configuration. Addr + Token are the worker's management
// credential (and, with a narrower token, the API's minter); UIURL is portal-facing (the base of the
// "View in Vault" links) and never used to talk to the API.
type Settings struct {
	// Addr is the Vault API address the worker and the in-cluster External Secrets Operator reach
	// Vault at, e.g. "http://vault:8200" (compose) or a host-reachable address (real clusters).
	Addr string
	// Mount is the KV v2 mount path (DefaultMount when empty).
	Mount string
	// Token authenticates API calls: the broad management token on the worker, or the narrow
	// token-minter token on the API. Never a Helm value inside a cluster.
	Token string
	// UIURL is the browser-facing base of the Vault UI ("https://vault.example/"), used to build the
	// handoff redirect. Defaults to Addr when unset.
	UIURL string
	// AuthMode is the Vault auth backend to configure (AuthLocal|AuthLDAP), mirroring KAAS_AUTH.
	AuthMode string
	// LDAP carries the directory settings when AuthMode==AuthLDAP, translated from the portal's own
	// authn/ldap config so Vault authenticates users against the same directory with the same filter.
	LDAP *LDAPAuth
	// TokenTTL bounds a minted handoff token's validity.
	TokenTTL time.Duration
}

// LDAPAuth is the subset of directory settings Vault's ldap auth method needs, translated from the
// portal's authn/ldap.Config by the caller (app.buildVaultManager) so the vault package stays
// decoupled from the authn package.
type LDAPAuth struct {
	URL          string // ldaps://dc.example.lab
	StartTLS     bool
	BindDN       string
	BindPassword string
	UserDN       string // user_base_dn
	UserAttr     string // the login attribute (sAMAccountName)
	InsecureTLS  bool
}

// Mount returns the KV mount, defaulting to DefaultMount.
func (s Settings) mount() string {
	if strings.TrimSpace(s.Mount) == "" {
		return DefaultMount
	}
	return strings.Trim(strings.TrimSpace(s.Mount), "/")
}

// Enabled reports whether a real Vault is configured (an address is set). The Fake is used otherwise.
func (s Settings) Enabled() bool { return strings.TrimSpace(s.Addr) != "" }

// Validate normalizes the settings and defaults. Called at startup so a half-configured Vault refuses
// to boot rather than failing every cluster at wiring time.
func (s Settings) Validate() (Settings, error) {
	s.Addr = strings.TrimSpace(s.Addr)
	s.Mount = s.mount()
	if s.UIURL == "" {
		s.UIURL = s.Addr
	}
	s.UIURL = strings.TrimRight(s.UIURL, "/")
	switch s.AuthMode {
	case "", AuthLocal:
		s.AuthMode = AuthLocal
	case AuthLDAP:
		if s.LDAP == nil || strings.TrimSpace(s.LDAP.URL) == "" {
			return s, fmt.Errorf("vault: AuthMode=ldap needs LDAP settings")
		}
	default:
		return s, fmt.Errorf("vault: unknown AuthMode %q (want local|ldap)", s.AuthMode)
	}
	if s.TokenTTL <= 0 {
		s.TokenTTL = 15 * time.Minute
	}
	return s, nil
}

// --- naming -------------------------------------------------------------------------------------
//
// Every Vault object the platform owns is named deterministically from a stable id, so the mapping
// is re-derivable and idempotent and the "kaas-" prefix keeps our objects clear of anything else in
// the Vault. Cluster and user/group ids are UUIDs, so no escaping is needed.

// ClusterPrefix is a cluster's KV subtree under the mount (no leading mount): "clusters/<id>".
func ClusterPrefix(clusterID string) string { return "clusters/" + clusterID }

// PolicyRead / PolicyWrite name a cluster's two policies.
func PolicyRead(clusterID string) string  { return "kaas-cluster-" + clusterID + "-read" }
func PolicyWrite(clusterID string) string { return "kaas-cluster-" + clusterID + "-write" }

// PolicyAdmin is the standing policy granting the whole mount, attached to the kaas-admins group.
const PolicyAdmin = "kaas-admin"

// GroupAdmins is the identity group every platform admin belongs to.
const GroupAdmins = "kaas-admins"

// GroupRead / GroupWrite name a platform group's two identity groups (one per role).
func GroupRead(groupID string) string  { return "kaas-group-" + groupID + "-read" }
func GroupWrite(groupID string) string { return "kaas-group-" + groupID + "-write" }

// EntityName names a user's Vault identity entity.
func EntityName(userID string) string { return "kaas-user-" + userID }

// AuthRole is the External-Secrets JWT auth role name for a cluster (bound to the read policy).
func AuthRole(clusterID string) string { return "kaas-eso-" + clusterID }

// ReadPolicyHCL / WritePolicyHCL are the policy documents for a cluster's subtree. Write grants full
// CRUD+list; read grants read+list. Both cover the KV v2 data and metadata paths (a KV v2 read is on
// data/…, a list is on metadata/…), so a reader can browse the tree and a writer can edit it.
func ReadPolicyHCL(mount, clusterID string) string {
	return policyHCL(mount, ClusterPrefix(clusterID), []string{"read", "list"})
}

func WritePolicyHCL(mount, clusterID string) string {
	return policyHCL(mount, ClusterPrefix(clusterID), []string{"create", "update", "read", "delete", "list"})
}

// AdminPolicyHCL grants the whole mount - the platform admins' standing access to every cluster's
// subtree and the platform/ section.
func AdminPolicyHCL(mount string) string {
	return fmt.Sprintf("path %q { capabilities = [\"create\",\"update\",\"read\",\"delete\",\"list\"] }\n",
		mount+"/*")
}

func policyHCL(mount, prefix string, caps []string) string {
	c := `"` + strings.Join(caps, `","`) + `"`
	var b strings.Builder
	fmt.Fprintf(&b, "path %q { capabilities = [%s] }\n", mount+"/data/"+prefix+"/*", c)
	fmt.Fprintf(&b, "path %q { capabilities = [%s] }\n", mount+"/metadata/"+prefix+"/*", c)
	fmt.Fprintf(&b, "path %q { capabilities = [\"list\",\"read\"] }\n", mount+"/metadata/"+prefix)
	return b.String()
}

// --- access model -------------------------------------------------------------------------------

// AccessSnapshot is the whole of the platform's authorization state, as read from the store by the
// leader-elected SyncAccess sweep. DesiredAccess turns it into the Vault objects that mirror it.
type AccessSnapshot struct {
	Users    []*domain.User
	Groups   []*domain.Group
	Clusters []*domain.Cluster // live clusters only (the caller filters out deleted ones)
}

// DesiredEntity is one Vault identity entity: a platform user, aliased by their username on the
// configured auth mount, carrying the write policies for the clusters they OWN (an owner always has
// full access to their own clusters, whatever their group roles).
type DesiredEntity struct {
	Name     string   // EntityName(user id)
	Alias    string   // the username the auth backend reports (the alias name)
	Policies []string // sorted, deduped
}

// DesiredGroup is one Vault identity group: a (platform-group, role) pair, the admins group, carrying
// the cluster policies its members should hold and listing its member entities by NAME (the real
// impl resolves names → entity ids).
type DesiredGroup struct {
	Name     string   // GroupRead/GroupWrite(group id), or GroupAdmins
	Members  []string // entity names
	Policies []string // sorted, deduped
}

// DesiredState is the full set of identity objects SyncAccess converges Vault to. Policies per
// cluster are provisioned by EnsureCluster, not here - this only attaches them.
type DesiredState struct {
	Entities []DesiredEntity
	Groups   []DesiredGroup
}

// DesiredAccess computes the Vault identity objects that mirror the platform's access model. It is a
// pure function of the snapshot - no Vault, no I/O - so the whole "who can read/write which cluster
// path" decision is unit-tested without a Vault, exactly like domain.EtcdDefragPolicy.
//
// The mapping is a direct transcription of app.accessTo:
//   - owner → write on their own clusters (entity policy).
//   - admins → the standing kaas-admins group with the whole-mount admin policy.
//   - group-mates → for a cluster X whose owner is in group G, every member of G gets access to X at
//     THEIR role in G. So X's write policy is carried by kaas-group-<G>-write and its read policy by
//     kaas-group-<G>-read, and a user lands in the -write or -read group per their own role in G.
func DesiredAccess(snap AccessSnapshot) DesiredState {
	usersByID := make(map[string]*domain.User, len(snap.Users))
	for _, u := range snap.Users {
		usersByID[u.ID] = u
	}

	// Per-entity owned-cluster write policies, and per-(group,role) cluster policies.
	entityPolicies := map[string]map[string]bool{} // user id -> policy set
	groupWritePol := map[string]map[string]bool{}  // group id -> write policy set
	groupReadPol := map[string]map[string]bool{}   // group id -> read policy set
	add := func(m map[string]map[string]bool, key, val string) {
		if m[key] == nil {
			m[key] = map[string]bool{}
		}
		m[key][val] = true
	}

	for _, c := range snap.Clusters {
		if c.OwnerID != "" {
			add(entityPolicies, c.OwnerID, PolicyWrite(c.ID))
		}
		owner := usersByID[c.OwnerID]
		if owner == nil {
			continue
		}
		// The clusters a group can reach are exactly those whose OWNER is in the group.
		for _, m := range owner.Memberships {
			add(groupWritePol, m.GroupID, PolicyWrite(c.ID))
			add(groupReadPol, m.GroupID, PolicyRead(c.ID))
		}
	}

	// Entities (every user), and the admins group membership.
	var entities []DesiredEntity
	var admins []string
	for _, u := range snap.Users {
		entities = append(entities, DesiredEntity{
			Name:     EntityName(u.ID),
			Alias:    strings.ToLower(u.Username),
			Policies: sortedKeys(entityPolicies[u.ID]),
		})
		if u.IsAdmin {
			admins = append(admins, EntityName(u.ID))
		}
	}
	sort.Strings(admins)

	// Identity groups: two per platform group (its write members / read members), plus kaas-admins.
	var groups []DesiredGroup
	groups = append(groups, DesiredGroup{Name: GroupAdmins, Members: admins, Policies: []string{PolicyAdmin}})
	for _, g := range snap.Groups {
		var writers, readers []string
		for _, u := range snap.Users {
			role, ok := u.RoleIn(g.ID)
			if !ok {
				continue
			}
			if role == domain.GroupRoleWrite {
				writers = append(writers, EntityName(u.ID))
			} else {
				readers = append(readers, EntityName(u.ID))
			}
		}
		sort.Strings(writers)
		sort.Strings(readers)
		groups = append(groups,
			DesiredGroup{Name: GroupWrite(g.ID), Members: writers, Policies: sortedKeys(groupWritePol[g.ID])},
			DesiredGroup{Name: GroupRead(g.ID), Members: readers, Policies: sortedKeys(groupReadPol[g.ID])},
		)
	}

	sort.Slice(entities, func(i, j int) bool { return entities[i].Name < entities[j].Name })
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return DesiredState{Entities: entities, Groups: groups}
}

// PoliciesForUser is the set of Vault policy names a user carries right now, across every cluster
// they can reach - what the "View in Vault" handoff token is minted with, so the token grants exactly
// the paths the portal would. Owner-write on own clusters, group-role on group-mates' clusters, and
// the admin policy for admins. A pure function, so the API can compute it without a round-trip.
func PoliciesForUser(actor *domain.User, clusters []*domain.Cluster, owners map[string]*domain.User) []string {
	if actor == nil {
		return nil
	}
	if actor.IsAdmin {
		return []string{PolicyAdmin}
	}
	set := map[string]bool{}
	for _, c := range clusters {
		if c.OwnerID == actor.ID {
			set[PolicyWrite(c.ID)] = true
			continue
		}
		owner := owners[c.OwnerID]
		if owner == nil {
			continue
		}
		best := domain.GroupRole("")
		for _, m := range actor.Memberships {
			if !owner.InGroup(m.GroupID) {
				continue
			}
			if m.Role == domain.GroupRoleWrite {
				best = domain.GroupRoleWrite
				break
			}
			best = domain.GroupRoleRead
		}
		switch best {
		case domain.GroupRoleWrite:
			set[PolicyWrite(c.ID)] = true
		case domain.GroupRoleRead:
			set[PolicyRead(c.ID)] = true
		}
	}
	return sortedKeys(set)
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- the seam -----------------------------------------------------------------------------------

// ESOAuth is the material Vault needs to trust a cluster's External Secrets Operator: the cluster's
// service-account token ISSUER and the PUBLIC signing keys (its /openid/v1/jwks), so Vault validates
// the ESO ServiceAccount token OFFLINE (no Vault→cluster reachability). Assembled by the reconciler
// from the cluster via config.Manager; empty in fake mode.
type ESOAuth struct {
	Issuer      string   // the cluster's SA token issuer (iss claim)
	PublicKeys  []string // PEM-encoded public keys from the cluster's JWKS
	ServiceAcct string   // the ESO ServiceAccount, "<namespace>/<name>"
}

// UIPath is the Vault UI deep-link for a cluster's KV subtree - where "View in Vault" lands. Vault's
// KV v2 UI lists a directory at ".../<mount>/kv/list/<path>/" (the "list" segment BEFORE the path,
// trailing slash for a directory) - not "<path>/list", which the UI reads as a secret name and 404s.
func (s Settings) UIPath(clusterID string) string {
	return fmt.Sprintf("%s/ui/vault/secrets/%s/kv/list/%s/",
		strings.TrimRight(s.UIURL, "/"), s.mount(), ClusterPrefix(clusterID))
}

// Manager is the Vault seam. The worker holds a management token and drives EnsurePlatform /
// EnsureCluster / ReleaseCluster / SyncAccess; the API holds a narrow minter token and calls only
// MintUserToken. Every write MUST be idempotent - EnsureCluster is a level-triggered upsert re-run on
// retry, ReleaseCluster runs on every deleting tick, SyncAccess re-runs on every sweep.
type Manager interface {
	// EnsurePlatform provisions the platform-wide objects: the KV mount, the platform/ section, the
	// auth backend matching the portal (userpass|ldap), the admin policy and the kaas-admins group.
	EnsurePlatform(ctx context.Context) error
	// EnsureCluster provisions a cluster's read/write policies, its KV subtree, and the External
	// Secrets JWT auth role (bound to the read policy). Idempotent.
	EnsureCluster(ctx context.Context, c *domain.Cluster, eso ESOAuth) error
	// ReleaseCluster removes a cluster's policies, auth role and KV subtree. Absence is success.
	ReleaseCluster(ctx context.Context, c *domain.Cluster) error
	// SyncAccess converges the identity entities/groups/policy-attachments to DesiredAccess(snap).
	SyncAccess(ctx context.Context, snap AccessSnapshot) error
	// MintUserToken issues a short-lived Vault token carrying policies, for the UI handoff.
	MintUserToken(ctx context.Context, policies []string, meta map[string]string) (token string, err error)
}

// --- Fake ---------------------------------------------------------------------------------------

// Fake is the default Manager: it performs no Vault I/O, records what it would have done, and logs.
// It keeps `make up-fake` (and every unit test) working with no Vault. Safe for concurrent use.
type Fake struct {
	Log *slog.Logger

	mu           sync.Mutex
	Platform     bool
	Clusters     map[string]bool // cluster id -> ensured (deleted on release)
	LastDesired  DesiredState
	MintedTokens int
}

// NewFake returns a Fake Manager.
func NewFake(log *slog.Logger) *Fake {
	return &Fake{Log: log, Clusters: map[string]bool{}}
}

func (f *Fake) logf(msg string, args ...any) {
	if f.Log != nil {
		f.Log.Info(msg, args...)
	}
}

func (f *Fake) EnsurePlatform(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Platform = true
	f.logf("vault(fake): ensured platform mount + auth backend")
	return nil
}

func (f *Fake) EnsureCluster(_ context.Context, c *domain.Cluster, _ ESOAuth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Clusters[c.ID] = true
	f.logf("vault(fake): ensured cluster path", "cluster", c.Name, "path", ClusterPrefix(c.ID))
	return nil
}

func (f *Fake) ReleaseCluster(_ context.Context, c *domain.Cluster) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Clusters, c.ID)
	f.logf("vault(fake): released cluster path", "cluster", c.Name, "path", ClusterPrefix(c.ID))
	return nil
}

func (f *Fake) SyncAccess(_ context.Context, snap AccessSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LastDesired = DesiredAccess(snap)
	f.logf("vault(fake): synced access", "entities", len(f.LastDesired.Entities), "groups", len(f.LastDesired.Groups))
	return nil
}

func (f *Fake) MintUserToken(_ context.Context, policies []string, _ map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MintedTokens++
	f.logf("vault(fake): minted handoff token", "policies", policies)
	return "fake-vault-token", nil
}
