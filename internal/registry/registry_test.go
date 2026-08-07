package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func testSettings() Settings {
	s, err := Settings{URL: "https://harbor.lab", Host: "harbor.lab", Mirror: true}.Validate()
	if err != nil {
		panic(err)
	}
	return s
}

func user(id, name string, admin bool, memberships ...domain.GroupMembership) *domain.User {
	return &domain.User{ID: id, Username: name, IsAdmin: admin, Memberships: memberships}
}

func member(groupID string, role domain.GroupRole) domain.GroupMembership {
	return domain.GroupMembership{GroupID: groupID, Role: role}
}

func cluster(id, name, owner string) *domain.Cluster {
	return &domain.Cluster{ID: id, Name: name, OwnerID: owner}
}

// roleOf finds the role a user holds on a project in the desired state.
func roleOf(t *testing.T, ds DesiredState, project, username string) Role {
	t.Helper()
	for _, m := range ds.Members {
		if m.Project == project && m.Username == username {
			return m.Role
		}
	}
	return ""
}

// TestDesiredAccessMirrorsPortalRoles is the core of the integration: what the registry permits has
// to be exactly what the portal's own accessTo would answer. Owner writes, a write-role group-mate
// pushes, a read-role group-mate only pulls, and an unrelated user gets nothing at all.
func TestDesiredAccessMirrorsPortalRoles(t *testing.T) {
	s := testSettings()
	owner := user("u-own", "owner", false, member("g1", domain.GroupRoleWrite))
	writer := user("u-w", "writer", false, member("g1", domain.GroupRoleWrite))
	reader := user("u-r", "reader", false, member("g1", domain.GroupRoleRead))
	stranger := user("u-s", "stranger", false, member("g2", domain.GroupRoleWrite))

	ds := DesiredAccess(AccessSnapshot{
		Users:    []*domain.User{owner, writer, reader, stranger},
		Groups:   []*domain.Group{{ID: "g1"}, {ID: "g2"}},
		Clusters: []*domain.Cluster{cluster("c1", "dev", "u-own")},
	}, s)

	project := s.prefix() + "dev"
	if got := roleOf(t, ds, project, "owner"); got != RoleProjectAdmin {
		t.Errorf("owner role = %q, want %q", got, RoleProjectAdmin)
	}
	if got := roleOf(t, ds, project, "writer"); got != RoleDeveloper {
		t.Errorf("write-role group-mate = %q, want %q (pull+push)", got, RoleDeveloper)
	}
	if got := roleOf(t, ds, project, "reader"); got != RoleGuest {
		t.Errorf("read-role group-mate = %q, want %q (pull only)", got, RoleGuest)
	}
	if got := roleOf(t, ds, project, "stranger"); got != "" {
		t.Errorf("unrelated user got %q on another tenant's project, want no membership", got)
	}
}

// TestDesiredAccessAdminsAreSystemAdmins pins the decision NOT to write a membership row per admin
// per cluster: an admin is a registry system admin instead, which is both what the portal means and
// what keeps the membership set from growing by O(admins x clusters).
func TestDesiredAccessAdminsAreSystemAdmins(t *testing.T) {
	s := testSettings()
	admin := user("u-a", "admin", true)
	owner := user("u-own", "owner", false)
	ds := DesiredAccess(AccessSnapshot{
		Users:    []*domain.User{admin, owner},
		Clusters: []*domain.Cluster{cluster("c1", "dev", "u-own")},
	}, s)

	var found bool
	for _, u := range ds.Users {
		if u.Username == "admin" {
			found = u.SysAdmin
		}
	}
	if !found {
		t.Error("platform admin is not a registry system admin")
	}
	if got := roleOf(t, ds, s.prefix()+"dev", "admin"); got != "" {
		t.Errorf("admin got an explicit membership %q; system admin should cover it", got)
	}
}

// TestDesiredAccessHighestRoleWins mirrors accessTo: a user sharing two groups with an owner holds
// the strongest of the roles, not the last one seen.
func TestDesiredAccessHighestRoleWins(t *testing.T) {
	s := testSettings()
	owner := user("u-own", "owner", false, member("g1", domain.GroupRoleRead), member("g2", domain.GroupRoleRead))
	both := user("u-b", "both", false, member("g1", domain.GroupRoleRead), member("g2", domain.GroupRoleWrite))
	ds := DesiredAccess(AccessSnapshot{
		Users:    []*domain.User{owner, both},
		Clusters: []*domain.Cluster{cluster("c1", "dev", "u-own")},
	}, s)
	if got := roleOf(t, ds, s.prefix()+"dev", "both"); got != RoleDeveloper {
		t.Errorf("role = %q, want %q (highest across shared groups)", got, RoleDeveloper)
	}
}

// TestProjectsForUserAgreesWithDesiredAccess is the anti-drift test: the portal's filter and the
// convergence are two readings of the same model, so a project the page shows at a role must be one
// the sweep would actually grant at that role.
func TestProjectsForUserAgreesWithDesiredAccess(t *testing.T) {
	s := testSettings()
	owner := user("u-own", "owner", false, member("g1", domain.GroupRoleWrite))
	reader := user("u-r", "reader", false, member("g1", domain.GroupRoleRead))
	clusters := []*domain.Cluster{cluster("c1", "dev", "u-own"), cluster("c2", "prod", "u-own")}
	snap := AccessSnapshot{Users: []*domain.User{owner, reader}, Clusters: clusters}
	ds := DesiredAccess(snap, s)

	owners := map[string]*domain.User{"u-own": owner}
	visible := ProjectsForUser(reader, clusters, owners, s)
	for _, c := range clusters {
		project := s.ClusterProject(c)
		want := roleOf(t, ds, project, "reader")
		if visible[project] != want {
			t.Errorf("page shows %q at %q but the sweep grants %q", project, visible[project], want)
		}
	}
	// The caches and the library are readable by everyone - every cluster already pulls from them.
	if visible[s.Library()] != RoleGuest {
		t.Error("library should be visible to every user")
	}
}

// TestProjectsForUserAdminSeesEverything pins nil as "no filter" rather than "nothing visible" -
// getting that backwards would silently empty an admin's Registry page.
func TestProjectsForUserAdminSeesEverything(t *testing.T) {
	s := testSettings()
	admin := user("u-a", "admin", true)
	if got := ProjectsForUser(admin, nil, nil, s); got != nil {
		t.Errorf("admin filter = %v, want nil (no filter)", got)
	}
}

// TestEnsureClusterDoesNotRotateAnExistingRobot is the contract the wiring depends on: EnsureCluster
// is level-triggered and re-runs, and a re-run that minted a fresh secret would invalidate the pull
// Secret every node of the cluster is already using.
func TestEnsureClusterDoesNotRotateAnExistingRobot(t *testing.T) {
	f := NewFake(nil, testSettings())
	c := cluster("c1", "dev", "u1")
	first, err := f.EnsureCluster(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Valid() {
		t.Fatal("first EnsureCluster returned no usable credential")
	}
	again, err := f.EnsureCluster(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if again.Secret != "" {
		t.Error("re-running EnsureCluster rotated the robot secret")
	}
	if again.Username != first.Username {
		t.Errorf("robot identity changed on re-run: %q -> %q", first.Username, again.Username)
	}
}

// TestNodeTrustMirrorsCarryTheProjectPath pins override_path's reason: Harbor's proxy cache lives
// UNDER a project path, so the mirror URL is not a bare host and containerd must be told not to
// append the upstream's own path to it.
func TestNodeTrustMirrors(t *testing.T) {
	s := testSettings()
	trust := s.NodeTrustFor("")
	if len(trust.Mirrors) != len(s.Upstreams) {
		t.Fatalf("mirrors = %d, want %d", len(trust.Mirrors), len(s.Upstreams))
	}
	for _, m := range trust.Mirrors {
		if m.Server == m.Mirror {
			t.Errorf("mirror %q equals its fallback server - the fallback must stay the upstream", m.Host)
		}
		if !strings.Contains(m.Mirror, "/v2/"+s.prefix()+"cache-") {
			t.Errorf("mirror %q does not point at a cache project: %s", m.Host, m.Mirror)
		}
	}
	// Mirror off is the retreat path: per-cluster projects still work, nodes are simply not pointed
	// at the cache. It must produce no mirror entries at all rather than empty ones.
	s.Mirror = false
	if got := s.NodeTrustFor(""); len(got.Mirrors) != 0 {
		t.Errorf("mirror disabled still produced %d mirrors", len(got.Mirrors))
	}
}

// TestValidateRejectsUnusableSettings: a bad prefix fails every project creation one cluster at a
// time, so it has to fail at start-up instead.
func TestValidateRejectsUnusableSettings(t *testing.T) {
	if _, err := (Settings{URL: "https://h", ProjectPrefix: "Kaas-"}).Validate(); err == nil {
		t.Error("uppercase project prefix accepted")
	}
	if _, err := (Settings{URL: "https://h", AuthMode: "oidc"}).Validate(); err == nil {
		t.Error("unknown auth mode accepted")
	}
	// Directory settings are required to WRITE the auth configuration...
	if _, err := (Settings{URL: "https://h", AuthMode: AuthLDAP, ManageAuth: true}).Validate(); err == nil {
		t.Error("ldap auth mode accepted with no directory settings to configure the registry with")
	}
	// ...but NOT to know which mode this deployment is in. This is the worker's shape: it is told the
	// mode so SyncAccess does not mint a local account per directory user, and withheld the bind
	// password so the container holding the libvirt socket never sees it. Rejecting it here is what
	// forced the worker to call itself "local", which is the bug in
	// TestRegistryKeepsLdapModeWithoutDirectoryConfig.
	if _, err := (Settings{URL: "https://h", AuthMode: AuthLDAP}).Validate(); err != nil {
		t.Errorf("ldap mode without directory settings must be legal where auth is not managed: %v", err)
	}
}

// TestHostDefaultsFromURL: the node-facing host falls back to the platform's own, which is right
// whenever both share one route - and wrong loudly rather than silently when they do not.
func TestHostDefaultsFromURL(t *testing.T) {
	s, err := Settings{URL: "https://harbor.lab:8443/"}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "harbor.lab:8443" {
		t.Errorf("Host = %q, want harbor.lab:8443", s.Host)
	}
}

// TestUIPathsAreAbsoluteOrEmpty guards a whole class of bug: a relative href is resolved by the
// browser against the CURRENT page, so a link built from an unset UI address navigates back to the
// portal - which looks like a broken link to the registry rather than like missing configuration.
func TestUIPathsAreAbsoluteOrEmpty(t *testing.T) {
	configured, err := Settings{URL: "https://harbor.lab", UIURL: "https://harbor.lab"}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.UIProjectPath("kaas-dev"); !strings.HasPrefix(got, "https://") {
		t.Errorf("UIProjectPath = %q, want an absolute URL", got)
	}
	// No registry at all - the fake-mode shape, where there is no Harbor to open.
	if got := (Settings{}).UIProjectPath("kaas-dev"); got != "" {
		t.Errorf("UIProjectPath with no UI address = %q, want empty", got)
	}
}

// TestDefaultUpstreamTypesAreHarborAdapters pins each upstream's provider type to Harbor's own
// adapter list (`GET /api/v2.0/replication/adapters`). Nothing else checks these strings: they are
// opaque to us, only a live Harbor validates them, and an unknown one comes back as a bare
// `500 internal server error` with no hint as to which field was wrong. Two are counter-intuitive and
// were both wrong on the first real bring-up - ghcr.io is "github-ghcr", not "github", and quay.io has
// no adapter of its own so it is proxied as a plain "docker-registry".
func TestDefaultUpstreamTypesAreHarborAdapters(t *testing.T) {
	// The adapter names Harbor v2.15 reports.
	adapters := map[string]bool{
		"ali-acr": true, "aws-ecr": true, "azure-acr": true, "docker-hub": true,
		"docker-registry": true, "github-ghcr": true, "google-gcr": true, "harbor": true,
		"huawei-SWR": true, "jfrog-artifactory": true, "tencent-tcr": true, "volcengine-cr": true,
	}
	for _, u := range DefaultUpstreams {
		if !adapters[u.Type] {
			t.Errorf("upstream %q has type %q, which Harbor does not implement", u.Name, u.Type)
		}
		if u.Host == "" || u.Endpoint == "" || u.Name == "" {
			t.Errorf("upstream %q is incompletely specified: %+v", u.Name, u)
		}
	}
}
