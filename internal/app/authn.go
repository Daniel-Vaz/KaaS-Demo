package app

// Directory authentication (Active Directory / LDAP), the counterpart to the local accounts in
// internal/auth.
//
// TWO ORTHOGONAL AXES, matching the KAAS_INFRA_PROVIDERS / KAAS_PROVISIONER split (see CLAUDE.md):
//
//	KAAS_AUTH=local|ldap   the mechanism - does this deployment authenticate against a directory?
//	KAAS_LDAP=fake|real    the seam - a real DC, or the in-memory fake so `make up-fake` demos it
//
// They are separate because "authenticate against a directory" and "there is a directory to reach"
// are different questions: KAAS_AUTH=ldap KAAS_LDAP=fake is the whole AD flow - group seeding, JIT
// provisioning, membership sync - with no domain controller in sight.
//
// In ldap mode the seeded LOCAL admin still logs in. That is not a loose end, it is the design:
// `make kubeconfig` and deploy/teardown-clusters.sh authenticate as it over POST /auth/login, and a
// DC outage must not lock the platform out of its own control plane. See App.Login.

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/authn"
	authnfake "github.com/Daniel-Vaz/KaaS-demo/internal/authn/fake"
	authnldap "github.com/Daniel-Vaz/KaaS-demo/internal/authn/ldap"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// Auth modes (KAAS_AUTH).
const (
	AuthLocal = "local" // local accounts only; self-registration open (the default)
	AuthLDAP  = "ldap"  // directory accounts + the local break-glass admin; registration disabled
)

// authMode reports the configured mechanism, validated.
func authMode() (string, error) {
	m := strings.ToLower(strings.TrimSpace(getenv("KAAS_AUTH", AuthLocal)))
	switch m {
	case AuthLocal, AuthLDAP:
		return m, nil
	default:
		return "", fmt.Errorf("KAAS_AUTH %q unknown (want local|ldap)", m)
	}
}

// buildAuthenticator selects the directory seam. Returns nil in local mode - the App treats a nil
// Authn as "there is no directory", so local mode needs no second branch anywhere.
//
// The config file is parsed in BOTH fake and real modes, deliberately: it means `KAAS_LDAP=fake`
// validates the operator's real mapping rules and synthesizes a user per rule, so the config can be
// proven correct before it is ever pointed at a live DC.
func buildAuthenticator(log *slog.Logger) (authn.Authenticator, error) {
	mode, err := authMode()
	if err != nil {
		return nil, err
	}
	if mode == AuthLocal {
		return nil, nil
	}
	path := getenv("KAAS_LDAP_CONFIG", "/etc/kaas/ldap.yaml")
	cfg, err := authnldap.Load(path)
	if err != nil {
		return nil, fmt.Errorf("KAAS_AUTH=ldap: %w", err)
	}
	switch seam := strings.ToLower(getenv("KAAS_LDAP", "real")); seam {
	case "fake":
		dir := authnfake.New(cfg.Mappings())
		log.Warn("KAAS_LDAP=fake - authenticating against an IN-MEMORY directory, not a real one",
			"users", strings.Join(dir.Usernames(), ","), "password", authnfake.Password)
		return dir, nil
	case "real":
		return authnldap.New(cfg, log)
	default:
		return nil, fmt.Errorf("KAAS_LDAP %q unknown (want fake|real)", seam)
	}
}

// AuthConfig is what the login page needs BEFORE it can authenticate: which mechanism is in play,
// and whether to offer the register affordance at all.
//
// It has its own public endpoint (GET /auth/config) rather than riding on /catalog like
// ProviderInfo does, because /catalog requires a session - and this is precisely the question a
// client asks while it has none.
type AuthConfig struct {
	Mode string `json:"mode"` // "local" | "ldap"
	// RegistrationEnabled is false in ldap mode: accounts come from the directory on first login,
	// so a self-service form would create a local account the directory knows nothing about.
	RegistrationEnabled bool `json:"registration_enabled"`
}

// AuthConfig reports the deployment's authentication shape. Public - it deliberately reveals only
// which mechanism is configured, never the directory's address, base DN or mapping rules.
func (a *App) AuthConfig() AuthConfig {
	if a.Authn == nil {
		return AuthConfig{Mode: AuthLocal, RegistrationEnabled: true}
	}
	return AuthConfig{Mode: AuthLDAP, RegistrationEnabled: false}
}

// adminUsername is the seeded local admin's name - the break-glass account. Read from the same env
// var ensureAdminLocked seeds from, so the two can never disagree.
func adminUsername() string { return getenv("KAAS_ADMIN_USERNAME", "admin") }

// seededAdminName reports whether username is the break-glass admin's.
//
// This is a load-bearing check on the login path, not a nicety. KAAS_ADMIN_USERNAME is never run
// through validateUsername, so an operator can seed the admin as any name at all - including one
// that also exists in the directory. If it does, and the directory path were allowed to claim it,
// then anyone who knows KAAS_ADMIN_PASSWORD (which defaults to "admin") would authenticate as that
// real person. The local admin owns its name exclusively, in both directions.
func seededAdminName(username string) bool {
	return strings.EqualFold(strings.TrimSpace(username), strings.TrimSpace(adminUsername()))
}

// ensureDirectoryGroups creates a group for every configured mapping rule, so an admin can see and
// grant against the directory's groups before anyone has logged in - and so the first login has
// somewhere to put its claims.
//
// Called from New alongside ensureAdmin, under the SAME boot-only lock: every api replica runs this
// at once, and it is a read-then-write (does this rule's group exist? no? create it) that would
// otherwise double-create or fail on the unique index. Deliberately NOT LockAdmission - that one is
// taken per-login and per-admission, and coupling boot seeding to it would let a slow login stall a
// rolling restart.
//
// No-op when Authn is nil, which is what keeps the worker out of this: it calls New too, but its
// env omits KAAS_AUTH, so it never builds a directory and never seeds groups.
func (a *App) ensureDirectoryGroups() error {
	if a.Authn == nil {
		return nil
	}
	return a.Store.WithLock(store.LockUserSeed, a.ensureDirectoryGroupsLocked)
}

func (a *App) ensureDirectoryGroupsLocked() error {
	// Mappings are per RULE, and several rules may share a group (the "K8s-Eng reads, K8s-Eng-Admins
	// writes" shape). Seed each group once.
	seen := make(map[string]bool)
	for _, m := range a.Authn.Mappings() {
		if seen[m.GroupKey] {
			continue
		}
		seen[m.GroupKey] = true
		existing, err := a.Store.GetGroupBySource(domain.SourceLDAP, m.GroupKey)
		switch {
		case err == nil:
			// The rule already owns a group. Follow a display-name change in the config - the rule's
			// key is the identity, the name is just its label.
			if existing.Name == m.Group {
				continue
			}
			a.Log.Info("directory group renamed by config", "group_key", m.GroupKey, "from", existing.Name, "to", m.Group)
			existing.Name = m.Group
			if err := a.Store.UpdateGroup(existing); err != nil {
				if errors.Is(err, store.ErrConflict) {
					return fmt.Errorf("ldap mapping %q: cannot rename its group to %q - another group already has that name", m.GroupKey, m.Group)
				}
				return fmt.Errorf("ldap mapping %q: rename group: %w", m.GroupKey, err)
			}
		case errors.Is(err, store.ErrNotFound):
			g := &domain.Group{
				ID:        newID(),
				Name:      m.Group,
				Source:    domain.SourceLDAP,
				SourceKey: m.GroupKey,
				CreatedAt: time.Now(),
			}
			// Store.CreateGroup directly, NOT App.CreateGroup: that one demands an admin actor (there
			// is none at boot) and enforces validateGroupName's 2–40 chars, which real AD group names
			// routinely exceed.
			if err := a.Store.CreateGroup(g); err != nil {
				if errors.Is(err, store.ErrConflict) {
					// Two ways to get here. Either a LOCAL group already holds this display name - in
					// which case adopting it would hand an admin's group to a config file, so we
					// refuse and make the operator choose a name - or we lost the boot race with
					// another replica, which the source_key index catches. Distinguish them, because
					// one is an operator error and the other is normal.
					if other, gerr := a.Store.GetGroupBySource(domain.SourceLDAP, m.GroupKey); gerr == nil {
						a.Log.Debug("directory group already seeded by another replica", "group_key", m.GroupKey, "group", other.Name)
						continue
					}
					return fmt.Errorf("ldap mapping %q: a group named %q already exists and is not managed by the directory - rename one of them", m.GroupKey, m.Group)
				}
				return fmt.Errorf("ldap mapping %q: create group: %w", m.GroupKey, err)
			}
			a.Log.Info("directory group seeded", "group_key", m.GroupKey, "group", m.Group, "role", m.Role)
		default:
			return fmt.Errorf("ldap mapping %q: look up group: %w", m.GroupKey, err)
		}
	}
	return nil
}

// syncDirectoryUser reconciles a just-authenticated directory identity into a portal account:
// creating it on first login, and recomputing its directory-driven group memberships every time.
//
// It runs under LockAdmission - NOT because memberships are quota, but because the users table IS
// the quota ledger and Store.UpdateUser rewrites the whole row. Without the lock, an admin saving a
// quota grant at the same moment this user logs in would have their grant silently overwritten by
// this function's pre-grant snapshot. Every writer of a user row shares one lock; see
// updateUserLocked, deleteUserLocked, deleteGroupLocked.
//
// The directory call has ALREADY happened by the time we get here, on purpose: dialling a DC while
// holding a platform-wide advisory lock (and a pgxpool connection) would let one hung domain
// controller stall every cluster admission on the platform.
func (a *App) syncDirectoryUser(id *authn.Identity) (*domain.User, error) {
	var out *domain.User
	err := a.Store.WithLock(store.LockAdmission, func() error {
		u, err := a.syncDirectoryUserLocked(id)
		out = u
		return err
	})
	return out, err
}

func (a *App) syncDirectoryUserLocked(id *authn.Identity) (*domain.User, error) {
	username, err := validateDirectoryUsername(id.Username)
	if err != nil {
		return nil, fmt.Errorf("directory account %q is unusable: %w", id.Username, err)
	}
	groups, err := a.groupsByID()
	if err != nil {
		return nil, err
	}
	claimed, err := a.claimedMemberships(id)
	if err != nil {
		return nil, err
	}

	u, err := a.Store.GetUserByUsername(username)
	if errors.Is(err, store.ErrNotFound) {
		// First login: provision the account. Zero quota - exactly like a self-registered account,
		// an admin grants capacity before they can build anything. No password hash: this account
		// is only ever authenticated by the directory.
		u = &domain.User{
			ID:          newID(),
			Username:    username,
			AuthSource:  domain.AuthSourceLDAP,
			DisplayName: id.DisplayName,
			Email:       id.Email,
			Memberships: claimed,
			CreatedAt:   time.Now(),
		}
		if err := a.Store.CreateUser(u); err != nil {
			return nil, fmt.Errorf("provision directory account %q: %w", username, err)
		}
		a.Log.Info("provisioned directory account", "username", username, "groups", len(claimed))
		return u, nil
	}
	if err != nil {
		return nil, err
	}
	if !u.FromDirectory() {
		// A local account already owns this name. Adopting it would let the directory take over an
		// account with a local password - including, if the operator ever pointed KAAS_ADMIN_USERNAME
		// at a real person's name, the admin. Refuse. The caller maps this to ErrInvalidCredentials
		// so it can't be used to probe which names are local.
		return nil, fmt.Errorf("%w: %q is a local account", errLocalAccountCollision, username)
	}
	u.DisplayName, u.Email = id.DisplayName, id.Email
	u.Memberships = mergeMemberships(claimed, u.Memberships, groups)
	if err := a.Store.UpdateUser(u); err != nil {
		return nil, err
	}
	return u, nil
}

// errLocalAccountCollision is a directory identity colliding with a local account. Never surfaced
// to a client - Login collapses it into ErrInvalidCredentials, because telling the caller "that one
// is local" is telling them which names to attack with a password guess instead.
var errLocalAccountCollision = errors.New("directory identity collides with a local account")

// directoryRuleFor reports whether a live mapping rule still claims this group's source key.
//
// This is what separates a directory group that is genuinely managed from one that has been
// ORPHANED by a config edit. Removing a rule deliberately does NOT delete its group - a typo in
// ldap.yaml must not be able to destroy a team and everyone's membership of it. But the group is
// then stranded: nothing recreates it, nothing syncs it, and the "it's managed by the directory"
// guard would refuse to let an admin remove it either. So an orphan is handed back to the admins to
// clean up (see deleteGroupLocked / RenameGroup); a claimed one stays theirs to leave alone.
func (a *App) directoryRuleFor(g *domain.Group) bool {
	if !g.DirectoryManaged() || a.Authn == nil {
		return false
	}
	for _, m := range a.Authn.Mappings() {
		if m.GroupKey == g.SourceKey {
			return true
		}
	}
	return false
}

// claimedMemberships resolves an identity's rule claims to portal group memberships.
//
// Two rules naming the same group are deduped HIGHEST-ROLE-WINS, matching how accessTo already
// resolves a user who reaches a cluster through several shared groups. Without the dedupe a user
// matching both a read rule and a write rule would produce duplicate memberships for one group,
// which UpdateUser rejects outright - i.e. they simply couldn't log in.
func (a *App) claimedMemberships(id *authn.Identity) ([]domain.GroupMembership, error) {
	byGroup := make(map[string]domain.GroupRole, len(id.Groups))
	for _, c := range id.Groups {
		g, err := a.Store.GetGroupBySource(domain.SourceLDAP, c.GroupKey)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// The directory matched a rule whose group was never seeded. Boot seeds every rule,
				// so this means the config changed under a running process; skip rather than fail
				// the login, and let the next boot seed it.
				a.Log.Warn("directory claim references an unseeded group", "group_key", c.GroupKey)
				continue
			}
			return nil, err
		}
		if existing, ok := byGroup[g.ID]; ok && existing == domain.GroupRoleWrite {
			continue // already the strongest role available
		}
		byGroup[g.ID] = c.Role
	}
	out := make([]domain.GroupMembership, 0, len(byGroup))
	for id, role := range byGroup {
		out = append(out, domain.GroupMembership{GroupID: id, Role: role})
	}
	sortMemberships(out)
	return out, nil
}
