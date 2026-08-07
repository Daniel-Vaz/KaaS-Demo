package app

// Container image registry wiring - the deployment-level selection of the registry seam, and the
// Registry page's read paths. Modeled on internal/app/vault.go, because it is the same problem: one
// central service next to the platform, per-cluster objects provisioned by the reconcile loop, and
// authorization mirrored out of Postgres. See internal/registry.
//
// Credential placement follows the same split as Vault, DNS and the LDAP bind password: the WORKER
// holds the registry ADMIN credential (it creates projects, robots and memberships and already holds
// every tenant's secrets), while the API holds only a narrow read-only robot for the Registry page.
// Both build from the same env - each calls only the half its credential permits. The Fake is used
// whenever KAAS_REGISTRY is unset, so `make up-fake` and the browser demo show the whole page with no
// registry at all.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	authnldap "github.com/Daniel-Vaz/KaaS-demo/internal/authn/ldap"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/registry"
	"github.com/Daniel-Vaz/KaaS-demo/internal/registry/harbor"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// registryFromEnv reads the deployment's registry settings. The auth backend the registry is
// configured with follows the portal's own KAAS_AUTH, exactly as Vault's does: local → the
// registry's own user database, ldap → the same directory the portal authenticates against.
func registryFromEnv() (registry.Settings, error) {
	s := registry.Settings{
		URL:           os.Getenv("KAAS_REGISTRY_URL"),
		Host:          os.Getenv("KAAS_REGISTRY_HOST"),
		UIURL:         getenv("KAAS_REGISTRY_UI_URL", os.Getenv("KAAS_REGISTRY_URL")),
		Username:      os.Getenv("KAAS_REGISTRY_USERNAME"),
		Password:      os.Getenv("KAAS_REGISTRY_PASSWORD"),
		ProjectPrefix: getenv("KAAS_REGISTRY_PROJECT_PREFIX", registry.DefaultProjectPrefix),
		// KAAS_REGISTRY_AUTH_MODE names the mode for the REGISTRY alone, falling back to the
		// platform-wide KAAS_AUTH. The indirection exists for the worker, which needs the mode
		// (SyncAccess must not mint a local account per user in a directory deployment) but must not
		// be handed KAAS_AUTH itself: that is read by every other seam too, and the Vault seam
		// rejects `ldap` outright unless it also has the directory settings - which the worker
		// deliberately never gets. Setting KAAS_AUTH there took the worker down at start-up with
		// `vault: AuthMode=ldap needs LDAP settings`, and a worker that will not start is a control
		// plane where nothing reconciles at all.
		AuthMode: strings.ToLower(getenv("KAAS_REGISTRY_AUTH_MODE", getenv("KAAS_AUTH", AuthLocal))),
		// Only a process that was TOLD how this deployment authenticates may write the registry's
		// identity configuration. An UNSET KAAS_AUTH is the worker's normal state - compose and the
		// chart pass it to the api alone, because the worker never authenticates a user - and reading
		// the "local" default as authoritative there would flip a directory-authenticated registry
		// back to its own user database and lock every user out. Absent means "no opinion", not
		// "local". See registry.Settings.ManageAuth.
		ManageAuth: os.Getenv("KAAS_AUTH") != "",
		// The mirror defaults ON when a registry is configured: the pull-through cache is most of the
		// value (see docs/deploy/integrations/registry.md), and it degrades to the upstream on its own
		// if the registry is unreachable. It is a separate switch from the registry itself precisely
		// so it can be turned off without giving up per-cluster projects.
		Mirror:         envBool("KAAS_REGISTRY_MIRROR", true),
		CAFile:         os.Getenv("KAAS_REGISTRY_CA_FILE"),
		Insecure:       envBool("KAAS_REGISTRY_INSECURE", false),
		RetainProject:  envBool("KAAS_REGISTRY_RETAIN_PROJECT", false),
		RobotTTL:       envDuration("KAAS_REGISTRY_ROBOT_TTL", 0),
		ProjectQuotaGB: envInt("KAAS_REGISTRY_PROJECT_QUOTA_GB", 0),
	}
	if hub := os.Getenv("KAAS_REGISTRY_DOCKERHUB_USERNAME"); hub != "" {
		// Without this the proxy cache pulls from Docker Hub anonymously and the WHOLE fleet shares
		// one anonymous rate limit - which is one of the failures the cache exists to remove.
		s.UpstreamAuth = map[string]registry.UpstreamCredential{
			"dockerhub": {Username: hub, Password: os.Getenv("KAAS_REGISTRY_DOCKERHUB_PASSWORD")},
		}
	}
	if s.AuthMode == registry.AuthLDAP {
		ldapCfg, err := registryLDAPAuth()
		if err != nil {
			return registry.Settings{}, fmt.Errorf("KAAS_REGISTRY: %w", err)
		}
		s.LDAP = ldapCfg
		if ldapCfg == nil {
			// The directory was requested but its config is not readable here. This is the NORMAL
			// state on the worker, which is given the MODE but deliberately not the mounted
			// ldap.yaml, so the bind password stays out of the container holding the libvirt socket
			// and every tenant's secrets (see CLAUDE.md, "Directory authentication").
			//
			// Stand down from managing auth - writing it from here would flip a correctly configured
			// ldap_auth back to db_auth and lock every user out - but KEEP the mode. It used to fall
			// back to `local` as well, purely to satisfy Validate, and that was a real bug rather
			// than a cosmetic one: AuthMode==local is what makes SyncAccess mint a LOCAL account per
			// user, so the worker quietly created a shadow account for every directory user, and
			// since a registry refuses to change auth mode once its database holds users, the first
			// sweep permanently locked the registry into db_auth. Projects, robots and memberships
			// all still converge either way.
			s.ManageAuth = false
		}
	}
	return s.Validate()
}

// registryLDAPAuth translates the portal's directory config (the same ldap.yaml the API mounts) into
// the subset the registry's LDAP auth needs, so a user's one set of directory credentials works in
// the portal, in Vault and in the registry. Returns nil without error when the file is absent.
func registryLDAPAuth() (*registry.LDAPAuth, error) {
	path := getenv("KAAS_LDAP_CONFIG", "/etc/kaas/ldap.yaml")
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	cfg, err := authnldap.Load(path)
	if err != nil {
		return nil, err
	}
	url := ""
	if len(cfg.URLs) > 0 {
		url = cfg.URLs[0]
	}
	return &registry.LDAPAuth{
		URL:          url,
		BindDN:       cfg.BindDN,
		BindPassword: os.Getenv(cfg.BindEnvVar),
		BaseDN:       cfg.UserBaseDN,
		UID:          getenv2(cfg.UsernameAttr, "sAMAccountName"),
		VerifyCert:   !cfg.InsecureSkipVerify,
	}, nil
}

// registryNodeTrust assembles what a cluster node needs to pull through the registry: the
// image-reference host, the CA to trust, and the containerd mirror list. It is a pure function of
// the settings plus the CA file - no registry round-trip and no minted credential - which is exactly
// what lets every worker replica render the same thing without coordinating, and what lets a
// non-leader replica configure a node's containerd. See registry.NodeTrust.
//
// A registry configured in FAKE mode contributes nothing: there is no registry to pull through, and
// pointing a real cluster's containerd at a host that answers nothing would slow every pull down by
// a failed connection attempt before falling back.
func registryNodeTrust(s registry.Settings) (registry.NodeTrust, error) {
	if !s.Enabled() || strings.ToLower(getenv("KAAS_REGISTRY", "fake")) != "real" {
		return registry.NodeTrust{}, nil
	}
	caPEM := ""
	if s.CAFile != "" {
		b, err := os.ReadFile(s.CAFile)
		if err != nil {
			// Failing here is deliberate: a missing CA means every node would fail TLS against the
			// registry, one cluster at a time, at bring-up. Refusing to start says so once.
			return registry.NodeTrust{}, fmt.Errorf("KAAS_REGISTRY_CA_FILE: %w", err)
		}
		caPEM = string(b)
	}
	return s.NodeTrustFor(caPEM), nil
}

// buildRegistry selects the registry seam from KAAS_REGISTRY (fake|real). One constructor for both
// halves: the Fake and the Harbor client each implement Manager AND Querier, and which half a
// process may actually use is decided by the credential it was given, not by the type.
func buildRegistry(log *slog.Logger, sink events.Sink, s registry.Settings) (registry.Manager, registry.Querier, error) {
	switch strings.ToLower(getenv("KAAS_REGISTRY", "fake")) {
	case "fake", "":
		f := registry.NewFake(log, s)
		return f, f, nil
	case "real":
		if !s.Enabled() {
			return nil, nil, fmt.Errorf("KAAS_REGISTRY=real needs KAAS_REGISTRY_URL")
		}
		if s.Username == "" || s.Password == "" {
			return nil, nil, fmt.Errorf("KAAS_REGISTRY=real needs KAAS_REGISTRY_USERNAME and KAAS_REGISTRY_PASSWORD")
		}
		c, err := harbor.New(harbor.Config{Settings: s, Events: sink, Log: log})
		if err != nil {
			return nil, nil, err
		}
		return c, c, nil
	default:
		return nil, nil, fmt.Errorf("unknown KAAS_REGISTRY %q (want fake|real)", os.Getenv("KAAS_REGISTRY"))
	}
}

// --- the Registry page ---------------------------------------------------------------------

// ErrRegistryPasswordUnavailable is "generate me a registry password" on a deployment where the
// directory owns the account. 409, like ErrClusterNotReady: the request is well-formed, the
// deployment is simply not in that mode.
var ErrRegistryPasswordUnavailable = errors.New("this deployment authenticates the registry against the directory - sign in with your directory password")

// RegistrySummary is the Registry page's payload: the registry's own health plus the projects the
// actor may see, each with the role they hold on it.
type RegistrySummary struct {
	Status   registry.Status    `json:"status"`
	Projects []registry.Project `json:"projects"`
}

// RegistryOverview returns the registry status and the actor's visible projects.
//
// Filtering is done SERVER-SIDE from the platform's own model (registry.ProjectsForUser) rather than
// by querying the registry as the acting user - the same trade the Monitoring, Security and Audit
// seams make, and for the same reason: the API holds one read-only credential and the platform is
// the authority on who may see what. The filter is the same function the convergence sweep uses, so
// the page cannot show a project the registry would refuse.
func (a *App) RegistryOverview(ctx context.Context, actor *domain.User) (*RegistrySummary, error) {
	if a.Registry == nil {
		return &RegistrySummary{Status: registry.Status{}}, nil
	}
	st := a.Registry.Status(ctx)
	out := &RegistrySummary{Status: st}
	if !st.Configured {
		return out, nil
	}
	projects, err := a.Registry.Projects(ctx)
	if err != nil {
		// A registry that is up enough to answer /health but not /projects should degrade to a page
		// that says so, not a 500 - the status block already carries the explanation.
		out.Status.Healthy = false
		out.Status.Message = err.Error()
		return out, nil
	}
	visible, err := a.visibleProjects(actor)
	if err != nil {
		return nil, err
	}
	byName, err := a.clusterProjectIndex()
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if visible != nil {
			role, ok := visible[p.Name]
			if !ok {
				continue
			}
			p.Role = role
		} else {
			p.Role = registry.RoleProjectAdmin // an admin is a registry system admin
		}
		if c, ok := byName[p.Name]; ok {
			p.ClusterID, p.ClusterName = c.ID, c.Name
		}
		out.Projects = append(out.Projects, p)
	}
	sort.Slice(out.Projects, func(i, j int) bool {
		// Cluster projects first (the ones a tenant cares about), then the platform's own.
		if out.Projects[i].Kind != out.Projects[j].Kind {
			return kindRank(out.Projects[i].Kind) < kindRank(out.Projects[j].Kind)
		}
		return out.Projects[i].Name < out.Projects[j].Name
	})
	return out, nil
}

func kindRank(k registry.ProjectKind) int {
	switch k {
	case registry.KindCluster:
		return 0
	case registry.KindLibrary:
		return 1
	case registry.KindCache:
		return 2
	}
	return 3
}

// RegistryRepositories lists a project's repositories, after checking the actor may see the project.
func (a *App) RegistryRepositories(ctx context.Context, actor *domain.User, project string) ([]registry.Repository, error) {
	if err := a.authorizeProject(actor, project); err != nil {
		return nil, err
	}
	return a.Registry.Repositories(ctx, project)
}

// RegistryArtifacts lists one repository's artifacts (tags, sizes, scan summary).
func (a *App) RegistryArtifacts(ctx context.Context, actor *domain.User, project, repo string) ([]registry.Artifact, error) {
	if err := a.authorizeProject(actor, project); err != nil {
		return nil, err
	}
	return a.Registry.Artifacts(ctx, project, repo)
}

// authorizeProject is the tenancy gate for every registry read. A project the actor cannot see is
// ErrNotFound rather than ErrForbidden - the same choice authorizeCluster makes, so the API never
// confirms that another tenant's cluster exists.
func (a *App) authorizeProject(actor *domain.User, project string) error {
	if a.Registry == nil {
		return store.ErrNotFound
	}
	visible, err := a.visibleProjects(actor)
	if err != nil {
		return err
	}
	if visible == nil {
		return nil // platform admin
	}
	if _, ok := visible[project]; !ok {
		return store.ErrNotFound
	}
	return nil
}

// visibleProjects resolves the actor's project→role map from the platform's own state. nil means
// "no filter" (a platform admin).
func (a *App) visibleProjects(actor *domain.User) (map[string]registry.Role, error) {
	if actor != nil && actor.IsAdmin {
		return nil, nil
	}
	clusters, err := a.Store.ListClusters()
	if err != nil {
		return nil, err
	}
	live := clusters[:0:0]
	owners := map[string]*domain.User{}
	for _, c := range clusters {
		if c.Phase == domain.PhaseDeleted {
			continue
		}
		live = append(live, c)
		if _, ok := owners[c.OwnerID]; !ok && c.OwnerID != "" {
			if u, err := a.Store.GetUser(c.OwnerID); err == nil {
				owners[c.OwnerID] = u
			}
		}
	}
	return registry.ProjectsForUser(actor, live, owners, a.registrySettings), nil
}

// clusterProjectIndex maps a project name back to its cluster, so the page can link a project to the
// cluster it belongs to. Built from the platform's own rows rather than from anything the registry
// reports - the registry knows nothing about clusters.
func (a *App) clusterProjectIndex() (map[string]*domain.Cluster, error) {
	clusters, err := a.Store.ListClusters()
	if err != nil {
		return nil, err
	}
	out := map[string]*domain.Cluster{}
	for _, c := range clusters {
		if c.Phase == domain.PhaseDeleted {
			continue
		}
		out[a.registrySettings.ClusterProject(c)] = c
	}
	return out, nil
}

// ClusterRegistry is one cluster's registry facts, rendered on the cluster detail page beside its
// DNS and Vault facts.
type ClusterRegistry struct {
	Configured bool   `json:"configured"`
	Wired      bool   `json:"wired"`
	Project    string `json:"project"`
	// PushPrefix is what a user types: "<host>/<project>". The full push command is built from it in
	// the portal, so the image name stays the user's own.
	PushPrefix string `json:"push_prefix"`
	UIURL      string `json:"ui_url,omitempty"`
	// CanPush is true when the actor holds write access on the cluster - the same gate that decides
	// whether they may manage it at all.
	CanPush bool `json:"can_push"`
}

// ClusterRegistry returns the cluster's project and push prefix. View-scoped: any group-mate who can
// see the cluster can see where its images live, and CanPush reflects whether they may push there.
func (a *App) ClusterRegistry(ctx context.Context, actor *domain.User, id string) (*ClusterRegistry, error) {
	c, err := a.authorizeCluster(actor, id)
	if err != nil {
		return nil, err
	}
	out := &ClusterRegistry{
		Configured: a.Registry != nil && a.Registry.Status(ctx).Configured,
		Wired:      c.RegistryWired,
		Project:    a.registrySettings.ClusterProject(c),
		PushPrefix: a.registrySettings.PullReference(c),
		CanPush:    a.accessTo(actor, c) == accessFull,
	}
	if out.Wired {
		out.UIURL = a.registrySettings.UIProjectPath(out.Project)
	}
	return out, nil
}

// RegistryCredential is the one-time registry password handed to its owner.
type RegistryCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Host     string `json:"host"`
}

// RotateRegistryPassword generates a registry password for the ACTOR (never for anyone else),
// applies it, and returns it once.
//
// The platform stores nothing: local accounts here carry bcrypt hashes, so there is no plaintext to
// copy into the registry, and inventing a place to keep one would be strictly worse than letting the
// user hold it. Re-running rotates. In directory mode there is nothing to generate - the directory
// owns the credential - so it refuses rather than creating a second, divergent password.
func (a *App) RotateRegistryPassword(ctx context.Context, actor *domain.User) (*RegistryCredential, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	if a.RegistryAdmin == nil || !a.registrySettings.CanSetPasswords() {
		return nil, ErrRegistryPasswordUnavailable
	}
	password := registry.GeneratePassword()
	username := strings.ToLower(actor.Username)
	if err := a.RegistryAdmin.SetUserPassword(ctx, username, password); err != nil {
		return nil, err
	}
	return &RegistryCredential{Username: username, Password: password, Host: a.registrySettings.Host}, nil
}
