package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/auth"
	"github.com/Daniel-Vaz/KaaS-demo/internal/authn"
	authnfake "github.com/Daniel-Vaz/KaaS-demo/internal/authn/fake"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// testMappings is a two-rule directory: one write group, one read group. The fake synthesizes
// ad-platform-write, ad-eng-read, ad-everyone (both) and ad-nobody (neither).
var testMappings = []authn.Mapping{
	{GroupKey: "platform", Group: "Platform", Role: domain.GroupRoleWrite},
	{GroupKey: "eng", Group: "Engineering", Role: domain.GroupRoleRead},
}

// newDirectoryApp is newTenancyApp plus a fake directory, seeded exactly as New would: admin first,
// then the directory's groups.
func newDirectoryApp(t *testing.T) *App {
	t.Helper()
	a := newTenancyApp(t)
	a.Authn = authnfake.New(testMappings)
	if err := a.ensureDirectoryGroups(); err != nil {
		t.Fatalf("ensureDirectoryGroups: %v", err)
	}
	return a
}

// groupNamed resolves a group by display name.
func groupNamed(t *testing.T, a *App, name string) *domain.Group {
	t.Helper()
	groups, err := a.Store.ListGroups()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("no group named %q", name)
	return nil
}

// roleIn is a user's role in the named group, or "" if they aren't a member.
func roleIn(t *testing.T, a *App, u *domain.User, groupName string) domain.GroupRole {
	t.Helper()
	g := groupNamed(t, a, groupName)
	fresh, err := a.Store.GetUser(u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	role, _ := fresh.RoleIn(g.ID)
	return role
}

func TestEnsureDirectoryGroupsSeedsOnePerRule(t *testing.T) {
	a := newDirectoryApp(t)
	for _, m := range testMappings {
		g := groupNamed(t, a, m.Group)
		if !g.DirectoryManaged() {
			t.Errorf("group %q source = %q, want directory-managed", m.Group, g.Source)
		}
		if g.SourceKey != m.GroupKey {
			t.Errorf("group %q source_key = %q, want %q", m.Group, g.SourceKey, m.GroupKey)
		}
	}
}

// Every replica seeds at boot, so seeding must be idempotent - not "create once and hope".
func TestEnsureDirectoryGroupsIsIdempotent(t *testing.T) {
	a := newDirectoryApp(t)
	before, _ := a.Store.ListGroups()
	for range 3 {
		if err := a.ensureDirectoryGroups(); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
	}
	after, _ := a.Store.ListGroups()
	if len(before) != len(after) {
		t.Fatalf("groups grew from %d to %d across re-seeds", len(before), len(after))
	}
}

// Relabelling a group in the config (same group_key, new `group:`) must rename the SAME group, not
// fork a second one - which is the whole reason groups are keyed on group_key rather than the name.
func TestEnsureDirectoryGroupsFollowsRename(t *testing.T) {
	a := newDirectoryApp(t)
	original := groupNamed(t, a, "Platform")

	a.Authn = authnfake.New([]authn.Mapping{
		{GroupKey: "platform", Group: "Platform Engineering", Role: domain.GroupRoleWrite},
		{GroupKey: "eng", Group: "Engineering", Role: domain.GroupRoleRead},
	})
	if err := a.ensureDirectoryGroups(); err != nil {
		t.Fatalf("re-seed after rename: %v", err)
	}

	renamed := groupNamed(t, a, "Platform Engineering")
	if renamed.ID != original.ID {
		t.Errorf("rename forked a new group (%s -> %s); it must relabel the same row", original.ID, renamed.ID)
	}
	groups, _ := a.Store.ListGroups()
	if len(groups) != 2 {
		t.Errorf("got %d groups after a rename, want 2", len(groups))
	}
}

// A config file must not be able to take over a group an admin created by hand.
func TestEnsureDirectoryGroupsRefusesToHijackLocalGroup(t *testing.T) {
	a := newTenancyApp(t)
	if _, err := a.CreateGroup(admin(t, a), "Platform"); err != nil {
		t.Fatalf("create local group: %v", err)
	}
	a.Authn = authnfake.New(testMappings)

	err := a.ensureDirectoryGroups()
	if err == nil {
		t.Fatal("seeding must refuse when a local group already holds the mapping's name")
	}
	if g := groupNamed(t, a, "Platform"); g.DirectoryManaged() {
		t.Error("the local group was hijacked by the directory")
	}
}

func TestDirectoryLoginProvisionsAccount(t *testing.T) {
	a := newDirectoryApp(t)
	u, err := a.Login(t.Context(), "ad-platform-write", authnfake.Password, "")
	if err != nil {
		t.Fatalf("directory login: %v", err)
	}
	if !u.FromDirectory() {
		t.Errorf("auth_source = %q, want ldap", u.AuthSource)
	}
	if u.PasswordHash != "" {
		t.Error("a directory account must not carry a password hash")
	}
	if u.IsAdmin {
		t.Error("a directory account must never be a platform admin")
	}
	// Zero quota until an admin grants: same as a self-registered account.
	if len(u.Quotas) != 0 {
		t.Errorf("Quotas = %v, want none", u.Quotas)
	}
	if got := roleIn(t, a, u, "Platform"); got != domain.GroupRoleWrite {
		t.Errorf("role in Platform = %q, want write", got)
	}
	if got := roleIn(t, a, u, "Engineering"); got != "" {
		t.Errorf("role in Engineering = %q, want no membership", got)
	}
}

func TestDirectoryLoginIsIdempotent(t *testing.T) {
	a := newDirectoryApp(t)
	first, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "")
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	second, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "")
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("logging in twice created two accounts (%s, %s)", first.ID, second.ID)
	}
	users, _ := a.Store.ListUsers()
	if len(users) != 2 { // the seeded admin + ad-eng
		t.Errorf("got %d users, want 2", len(users))
	}
}

// A user matching several rules lands in every group, each with its own role.
func TestDirectoryLoginMultipleGroups(t *testing.T) {
	a := newDirectoryApp(t)
	u, err := a.Login(t.Context(), "ad-everyone", authnfake.Password, "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := roleIn(t, a, u, "Platform"); got != domain.GroupRoleWrite {
		t.Errorf("role in Platform = %q, want write", got)
	}
	if got := roleIn(t, a, u, "Engineering"); got != domain.GroupRoleRead {
		t.Errorf("role in Engineering = %q, want read", got)
	}
}

// Matching no rule is a valid account, not a failed login: they still own their own clusters.
func TestDirectoryLoginNoGroups(t *testing.T) {
	a := newDirectoryApp(t)
	u, err := a.Login(t.Context(), "ad-nobody", authnfake.Password, "")
	if err != nil {
		t.Fatalf("a user matching no rule must still be able to log in: %v", err)
	}
	if len(u.Memberships) != 0 {
		t.Errorf("Memberships = %v, want none", u.Memberships)
	}
}

// sharedGroupMappings is the common Active Directory shape: ONE team, where a subset may write.
// Two rules, one group_key - so both roles land in the same portal group and the whole team shares
// cluster access.
var sharedGroupMappings = []authn.Mapping{
	{GroupKey: "eng", Group: "Engineering", Role: domain.GroupRoleRead},
	{GroupKey: "eng", Group: "Engineering", Role: domain.GroupRoleWrite},
}

// Several rules pointing at one group must produce ONE group, not one per rule.
func TestSharedGroupSeedsOnce(t *testing.T) {
	a := newTenancyApp(t)
	a.Authn = authnfake.New(sharedGroupMappings)
	if err := a.ensureDirectoryGroups(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	groups, _ := a.Store.ListGroups()
	if len(groups) != 1 {
		t.Fatalf("got %d groups for two rules sharing a group_key, want 1", len(groups))
	}
	if groups[0].SourceKey != "eng" {
		t.Errorf("source_key = %q, want eng", groups[0].SourceKey)
	}
}

// Members matched by different rules land in the SAME group with their own roles - which is the
// entire point: they can see each other's clusters, but only one of them can change them.
func TestSharedGroupGivesEachRuleItsOwnRole(t *testing.T) {
	a := newTenancyApp(t)
	a.Authn = authnfake.New(sharedGroupMappings)
	if err := a.ensureDirectoryGroups(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reader, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "")
	if err != nil {
		t.Fatalf("reader login: %v", err)
	}
	writer, err := a.Login(t.Context(), "ad-eng-write", authnfake.Password, "")
	if err != nil {
		t.Fatalf("writer login: %v", err)
	}
	if got := roleIn(t, a, reader, "Engineering"); got != domain.GroupRoleRead {
		t.Errorf("reader role = %q, want read", got)
	}
	if got := roleIn(t, a, writer, "Engineering"); got != domain.GroupRoleWrite {
		t.Errorf("writer role = %q, want write", got)
	}
	// Same group, so they are group-mates and share access.
	g := groupNamed(t, a, "Engineering")
	r, _ := mustGet(t, a, reader.ID).RoleIn(g.ID)
	w, _ := mustGet(t, a, writer.ID).RoleIn(g.ID)
	if r == "" || w == "" {
		t.Error("reader and writer must be members of the same group")
	}
}

// A user matching BOTH rules gets the strongest role, not a duplicate membership (which UpdateUser
// rejects outright - i.e. they simply couldn't log in). Mirrors accessTo's "highest role across
// shared groups".
func TestSharedGroupDedupesToHighestRole(t *testing.T) {
	a := newTenancyApp(t)
	a.Authn = authnfake.New(sharedGroupMappings)
	if err := a.ensureDirectoryGroups(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u, err := a.Login(t.Context(), "ad-everyone", authnfake.Password, "")
	if err != nil {
		t.Fatalf("a user matching both rules must log in: %v", err)
	}
	if len(u.Memberships) != 1 {
		t.Fatalf("Memberships = %v, want exactly one (deduped)", u.Memberships)
	}
	if got := roleIn(t, a, u, "Engineering"); got != domain.GroupRoleWrite {
		t.Errorf("role = %q, want write (highest wins)", got)
	}
}

func mustGet(t *testing.T, a *App, id string) *domain.User {
	t.Helper()
	u, err := a.Store.GetUser(id)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	return u
}

// The seeded local admin must authenticate with its own password even in directory mode: it is the
// break-glass account, and `make kubeconfig` / teardown-clusters.sh log in as it.
func TestLocalAdminStillLogsInUnderDirectoryAuth(t *testing.T) {
	a := newDirectoryApp(t)
	u, err := a.Login(t.Context(), "admin", "admin", "")
	if err != nil {
		t.Fatalf("break-glass admin login failed under directory auth: %v", err)
	}
	if !u.IsAdmin || u.FromDirectory() {
		t.Errorf("resolved to the wrong account: is_admin=%v auth_source=%q", u.IsAdmin, u.AuthSource)
	}
}

// The admin's username is owned by the local account exclusively. If the directory could claim it,
// an operator who set KAAS_ADMIN_USERNAME to a real person's name would hand platform admin to
// anyone holding KAAS_ADMIN_PASSWORD (which defaults to "admin").
func TestDirectoryCannotClaimTheSeededAdminUsername(t *testing.T) {
	t.Setenv("KAAS_ADMIN_USERNAME", "ad-platform-write")
	a := newTenancyApp(t) // seeds a LOCAL admin named ad-platform
	a.Authn = authnfake.New(testMappings)
	if err := a.ensureDirectoryGroups(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The directory has a user by this name too, with this password. It must not get in.
	_, err := a.Login(t.Context(), "ad-platform-write", authnfake.Password, "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("directory login as the seeded admin name = %v, want ErrInvalidCredentials", err)
	}
	// The local password still works, and still resolves to the admin.
	u, err := a.Login(t.Context(), "ad-platform-write", "admin", "")
	if err != nil {
		t.Fatalf("local admin login: %v", err)
	}
	if !u.IsAdmin {
		t.Error("the local admin lost its admin flag")
	}
}

// A directory identity colliding with an existing local account must be refused - and must be
// indistinguishable from a wrong password, or it reveals which names are local.
func TestDirectoryIdentityCannotTakeOverLocalAccount(t *testing.T) {
	a := newDirectoryApp(t)
	if _, err := a.Register("ad-eng-read", "localpass"); err == nil {
		t.Fatal("Register must be disabled under directory auth")
	}
	// Create the collision the only other way it can happen: a local account that predates the
	// switch to directory auth.
	local := &domain.User{ID: newID(), Username: "ad-eng-read", PasswordHash: mustHash(t, "localpass"), AuthSource: domain.AuthSourceLocal}
	if err := a.Store.CreateUser(local); err != nil {
		t.Fatalf("seed local account: %v", err)
	}

	_, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("collision = %v, want an opaque ErrInvalidCredentials", err)
	}
	// The local password still works - the account was not taken over.
	u, err := a.Login(t.Context(), "ad-eng-read", "localpass", "")
	if err != nil {
		t.Fatalf("local login after collision: %v", err)
	}
	if u.FromDirectory() {
		t.Error("the local account was converted to a directory account")
	}
}

func TestRegisterDisabledUnderDirectoryAuth(t *testing.T) {
	a := newDirectoryApp(t)
	_, err := a.Register("newbie", "password")
	if !errors.Is(err, ErrRegistrationDisabled) {
		t.Fatalf("Register = %v, want ErrRegistrationDisabled", err)
	}
}

func TestRegisterStillWorksInLocalMode(t *testing.T) {
	a := newTenancyApp(t) // no Authn
	if _, err := a.Register("newbie", "password"); err != nil {
		t.Fatalf("Register must still work in local mode: %v", err)
	}
}

func TestAuthConfig(t *testing.T) {
	local := newTenancyApp(t)
	if got := local.AuthConfig(); got.Mode != AuthLocal || !got.RegistrationEnabled {
		t.Errorf("local AuthConfig = %+v, want local + registration on", got)
	}
	dir := newDirectoryApp(t)
	if got := dir.AuthConfig(); got.Mode != AuthLDAP || got.RegistrationEnabled {
		t.Errorf("directory AuthConfig = %+v, want ldap + registration off", got)
	}
}

// THE CLOBBER REGRESSION. An admin's quota grant and a directory login both rewrite the whole user
// row. The grant must survive the user's next login.
func TestDirectoryLoginPreservesAdminQuotaGrant(t *testing.T) {
	a := newDirectoryApp(t)
	u, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "")
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	grant := map[string]domain.ResourceQuota{domain.ProviderKVM: {VCPU: 8, MemMB: 16384}}
	if _, err := a.UpdateUser(admin(t, a), u.ID, UpdateUserRequest{Quotas: &grant}); err != nil {
		t.Fatalf("grant quota: %v", err)
	}

	if _, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, ""); err != nil {
		t.Fatalf("second login: %v", err)
	}

	fresh, _ := a.Store.GetUser(u.ID)
	if got := fresh.QuotaOn(domain.ProviderKVM); got.VCPU != 8 || got.MemMB != 16384 {
		t.Errorf("quota after re-login = %+v, want the admin's 8/16384 grant intact", got)
	}
}

// An admin adding a directory user to a LOCAL group must stick - and must survive the next login,
// since the directory has no opinion about local groups. This is the merge working in both
// directions at once.
func TestLocalMembershipSurvivesDirectorySync(t *testing.T) {
	a := newDirectoryApp(t)
	u, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	local, err := a.CreateGroup(admin(t, a), "Local Team")
	if err != nil {
		t.Fatalf("create local group: %v", err)
	}

	// The Admin page sends the user's ENTIRE membership list, directory memberships included.
	fresh, _ := a.Store.GetUser(u.ID)
	next := append([]domain.GroupMembership{}, fresh.Memberships...)
	next = append(next, domain.GroupMembership{GroupID: local.ID, Role: domain.GroupRoleWrite})
	if _, err := a.UpdateUser(admin(t, a), u.ID, UpdateUserRequest{Memberships: &next}); err != nil {
		t.Fatalf("add to local group: %v", err)
	}
	if got := roleIn(t, a, u, "Local Team"); got != domain.GroupRoleWrite {
		t.Fatalf("role in Local Team = %q, want write - the merge rejected a legitimate local add", got)
	}

	if _, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, ""); err != nil {
		t.Fatalf("re-login: %v", err)
	}
	if got := roleIn(t, a, u, "Local Team"); got != domain.GroupRoleWrite {
		t.Errorf("local membership after re-login = %q, want write - the sync clobbered it", got)
	}
	if got := roleIn(t, a, u, "Engineering"); got != domain.GroupRoleRead {
		t.Errorf("directory membership after re-login = %q, want read", got)
	}
}

// An admin cannot hand-edit a directory group's roster: the request's directory memberships are
// ignored, so removing one has no effect.
func TestAdminCannotEditDirectoryMemberships(t *testing.T) {
	a := newDirectoryApp(t)
	u, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	// Try to drop them from Engineering (a directory group) by sending an empty list.
	empty := []domain.GroupMembership{}
	if _, err := a.UpdateUser(admin(t, a), u.ID, UpdateUserRequest{Memberships: &empty}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := roleIn(t, a, u, "Engineering"); got != domain.GroupRoleRead {
		t.Errorf("directory membership = %q after an admin tried to remove it, want read (unchanged)", got)
	}
}

// A directory group's roster lives in the config, so renaming or deleting the group in the portal
// would be undone (or worse, forked) at the next boot.
func TestDirectoryGroupsAreNotEditable(t *testing.T) {
	a := newDirectoryApp(t)
	g := groupNamed(t, a, "Engineering")

	if _, err := a.RenameGroup(admin(t, a), g.ID, "Renamed"); !errors.Is(err, ErrForbidden) {
		t.Errorf("RenameGroup on a directory group = %v, want ErrForbidden", err)
	}
	if err := a.DeleteGroup(admin(t, a), g.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("DeleteGroup on a directory group = %v, want ErrForbidden", err)
	}
	// Local groups stay fully editable.
	local, err := a.CreateGroup(admin(t, a), "Local Team")
	if err != nil {
		t.Fatalf("create local group: %v", err)
	}
	if _, err := a.RenameGroup(admin(t, a), local.ID, "Renamed Team"); err != nil {
		t.Errorf("RenameGroup on a local group: %v", err)
	}
	if err := a.DeleteGroup(admin(t, a), local.ID); err != nil {
		t.Errorf("DeleteGroup on a local group: %v", err)
	}
}

// Removing a rule from the config must NOT delete its group - a typo in ldap.yaml cannot be allowed
// to destroy a team and everyone's membership of it. The group is left orphaned instead.
//
// But an orphan must then be an ADMIN's to clean up: nothing syncs or recreates it, so if the
// "managed by the directory" guard still refused a delete, the group would be stranded in the portal
// forever - read-only and undeletable. Found by driving the real API with a changed config.
func TestOrphanedDirectoryGroupBecomesEditable(t *testing.T) {
	a := newDirectoryApp(t)
	u, err := a.Login(t.Context(), "ad-platform-write", authnfake.Password, "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	g := groupNamed(t, a, "Platform")

	// While its rule exists, it is off-limits.
	if _, err := a.RenameGroup(admin(t, a), g.ID, "Nope"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("rename of a live directory group = %v, want ErrForbidden", err)
	}

	// The operator drops the platform rule from the config and restarts.
	a.Authn = authnfake.New([]authn.Mapping{
		{GroupKey: "eng", Group: "Engineering", Role: domain.GroupRoleRead},
	})
	if err := a.ensureDirectoryGroups(); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	// The group survives the config edit, with its members.
	if _, err := a.Store.GetGroup(g.ID); err != nil {
		t.Fatalf("removing a rule must not delete its group: %v", err)
	}
	if got := roleIn(t, a, u, "Platform"); got != domain.GroupRoleWrite {
		t.Errorf("member's role after the rule was removed = %q, want it untouched", got)
	}
	// ...and it is now the admin's to deal with.
	views, err := a.ListGroups(admin(t, a))
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	for _, v := range views {
		if v.Name == "Platform" && !v.Orphaned {
			t.Error("Platform should be reported as orphaned")
		}
		if v.Name == "Engineering" && v.Orphaned {
			t.Error("Engineering still has a rule and must not be orphaned")
		}
	}
	if _, err := a.RenameGroup(admin(t, a), g.ID, "Ex-Platform"); err != nil {
		t.Errorf("rename of an ORPHANED directory group: %v - it would be stranded otherwise", err)
	}
	if err := a.DeleteGroup(admin(t, a), g.ID); err != nil {
		t.Errorf("delete of an ORPHANED directory group: %v - it would be stranded otherwise", err)
	}
}

func TestDirectoryLoginBadPassword(t *testing.T) {
	a := newDirectoryApp(t)
	if _, err := a.Login(t.Context(), "ad-eng-read", "wrong", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("bad password = %v, want ErrInvalidCredentials", err)
	}
	if _, err := a.Login(t.Context(), "nobody-here", authnfake.Password, ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("unknown user = %v, want ErrInvalidCredentials", err)
	}
}

// An empty password must never authenticate. A real LDAP simple bind with an empty password
// succeeds as an unauthenticated bind, so this is the check standing between us and every account
// in the directory accepting a blank password.
func TestDirectoryLoginRejectsEmptyPassword(t *testing.T) {
	a := newDirectoryApp(t)
	if _, err := a.Login(t.Context(), "ad-eng-read", "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("empty password = %v, want ErrInvalidCredentials", err)
	}
}

func TestThrottleTripsAndResets(t *testing.T) {
	t.Setenv("KAAS_LDAP_MAX_FAILURES", "3")
	a := newDirectoryApp(t)

	for i := range 3 {
		if _, err := a.Login(t.Context(), "ad-eng-read", "wrong", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	// Fourth attempt is refused without reaching the directory at all.
	if _, err := a.Login(t.Context(), "ad-eng-read", "wrong", "10.0.0.1"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("4th attempt = %v, want ErrTooManyAttempts", err)
	}
	// Even the CORRECT password is refused while throttled - otherwise the throttle wouldn't be
	// stopping the bind, which is its whole purpose.
	if _, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "10.0.0.1"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("correct password while throttled = %v, want ErrTooManyAttempts", err)
	}

	// Clearing the counter lets them back in.
	if err := a.Store.ResetLoginFailures(store.ThrottleScopeUser, "ad-eng-read"); err != nil {
		t.Fatalf("reset user scope: %v", err)
	}
	if err := a.Store.ResetLoginFailures(store.ThrottleScopeIP, "10.0.0.1"); err != nil {
		t.Fatalf("reset ip scope: %v", err)
	}
	if _, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "10.0.0.1"); err != nil {
		t.Fatalf("login after reset: %v", err)
	}
}

// The per-username counter must hold even when the attacker rotates source addresses - that is the
// half that actually protects a directory account from being locked out.
func TestThrottleUserScopeSurvivesIPRotation(t *testing.T) {
	t.Setenv("KAAS_LDAP_MAX_FAILURES", "3")
	a := newDirectoryApp(t)
	for i, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		if _, err := a.Login(t.Context(), "ad-eng-read", "wrong", ip); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d from %s = %v", i+1, ip, err)
		}
	}
	if _, err := a.Login(t.Context(), "ad-eng-read", "wrong", "10.0.0.4"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("attempt from a fresh IP = %v, want ErrTooManyAttempts - the username counter must hold", err)
	}
}

// The two scopes MUST have different thresholds. A whole office shares one NAT egress address, so
// if the per-IP limit were the per-username limit (3), three unrelated typos by three colleagues
// would lock the entire building out of the portal.
//
// Caught by driving the real API: an end-to-end script hitting many usernames from one address
// tripped the IP counter almost immediately, which is exactly what a busy office looks like.
func TestThrottleIPScopeToleratesSharedEgress(t *testing.T) {
	t.Setenv("KAAS_LDAP_MAX_FAILURES", "3")     // per account
	t.Setenv("KAAS_LDAP_MAX_IP_FAILURES", "30") // per source address
	a := newDirectoryApp(t)

	const office = "203.0.113.7"
	// Ten colleagues, each fatfingering their password twice - well under the per-user limit, but
	// twenty failures from one address.
	for i := range 10 {
		user := fmt.Sprintf("colleague-%d", i)
		for range 2 {
			if _, err := a.Login(t.Context(), user, "typo", office); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("%s = %v, want ErrInvalidCredentials", user, err)
			}
		}
	}
	// An eleventh colleague, with the right password, must still get in.
	if _, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, office); err != nil {
		t.Fatalf("a correct login from a shared egress address after colleagues' typos: %v - the per-IP threshold is too low", err)
	}
}

// ...but the IP scope must still catch an actual spray: many usernames, one source, past the limit.
func TestThrottleIPScopeCatchesSpray(t *testing.T) {
	t.Setenv("KAAS_LDAP_MAX_FAILURES", "3")
	t.Setenv("KAAS_LDAP_MAX_IP_FAILURES", "10")
	a := newDirectoryApp(t)

	const attacker = "198.51.100.4"
	for i := range 10 {
		_, _ = a.Login(t.Context(), fmt.Sprintf("victim-%d", i), "guess", attacker)
	}
	// The 11th distinct username from this source is refused, even though that account has no
	// failures of its own - this is the only thing standing between a name list and the whole
	// directory.
	if _, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, attacker); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("spray from one source = %v, want ErrTooManyAttempts", err)
	}
}

// A successful login clears the counter, so a user who eventually remembers their password isn't
// left sitting out the window.
func TestThrottleResetsOnSuccess(t *testing.T) {
	t.Setenv("KAAS_LDAP_MAX_FAILURES", "3")
	a := newDirectoryApp(t)
	for range 2 {
		_, _ = a.Login(t.Context(), "ad-eng-read", "wrong", "10.0.0.1")
	}
	if _, err := a.Login(t.Context(), "ad-eng-read", authnfake.Password, "10.0.0.1"); err != nil {
		t.Fatalf("login on the 3rd attempt: %v", err)
	}
	count, _, _ := a.Store.LoginFailures(store.ThrottleScopeUser, "ad-eng-read")
	if count != 0 {
		t.Errorf("failures after a successful login = %d, want 0", count)
	}
}

// The break-glass admin is a LOCAL account, so it never touches the directory throttle.
func TestThrottleDoesNotApplyToLocalAdmin(t *testing.T) {
	t.Setenv("KAAS_LDAP_MAX_FAILURES", "1")
	a := newDirectoryApp(t)
	for range 3 {
		if _, err := a.Login(t.Context(), "admin", "wrong", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("bad admin password = %v, want ErrInvalidCredentials", err)
		}
	}
	if _, err := a.Login(t.Context(), "admin", "admin", "10.0.0.1"); err != nil {
		t.Fatalf("the break-glass admin must not be throttleable out of its own control plane: %v", err)
	}
}

func TestValidateDirectoryUsername(t *testing.T) {
	// AD names that validateUsername would reject but a real employee actually has.
	for _, ok := range []string{"dvaz", "d.vaz", "dvaz@example.lab", "svc_kaas", "user-1"} {
		if _, err := validateDirectoryUsername(ok); err != nil {
			t.Errorf("validateDirectoryUsername(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{"", "ab", "has space", "UPPER", "tab\there", "sem;colon"} {
		if _, err := validateDirectoryUsername(bad); err == nil {
			t.Errorf("validateDirectoryUsername(%q) must be rejected", bad)
		}
	}
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	h, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}
