package registry

import (
	"context"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// TestFakeShowsPlatformProjectsWithoutEnsurePlatform is the demo's contract. In fake mode nothing
// calls EnsurePlatform - the platform sweeps are leader-elected and there is no leader without
// Postgres - so a listing keyed on it would hide the pull-through caches, which are the most visible
// half of the feature, from `make up-fake` and the browser demo.
func TestFakeShowsPlatformProjectsWithoutEnsurePlatform(t *testing.T) {
	s := testSettings()
	f := NewFake(nil, s)

	projects, err := f.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[ProjectKind]int{}
	for _, p := range projects {
		kinds[p.Kind]++
	}
	if kinds[KindLibrary] != 1 {
		t.Errorf("library projects = %d, want 1", kinds[KindLibrary])
	}
	if kinds[KindCache] != len(s.Upstreams) {
		t.Errorf("cache projects = %d, want %d", kinds[KindCache], len(s.Upstreams))
	}
	if kinds[KindCluster] != 0 {
		t.Errorf("cluster projects = %d before any cluster exists, want 0", kinds[KindCluster])
	}
}

// TestFakeMirrorOffHidesCaches: with the mirror off there are no cache projects to show, so the page
// must not advertise them.
func TestFakeMirrorOffHidesCaches(t *testing.T) {
	s := testSettings()
	s.Mirror = false
	f := NewFake(nil, s)
	projects, _ := f.Projects(context.Background())
	for _, p := range projects {
		if p.Kind == KindCache {
			t.Errorf("cache project %q listed with the mirror disabled", p.Name)
		}
	}
}

// TestFakeClusterProjectAppearsAndIsReleased: a cluster's project tracks its lifecycle, unlike the
// platform's own - which is exactly why the two are sourced differently.
func TestFakeClusterProjectAppearsAndIsReleased(t *testing.T) {
	s := testSettings()
	f := NewFake(nil, s)
	c := &domain.Cluster{ID: "c1", Name: "dev"}

	if _, err := f.EnsureCluster(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if !hasProject(t, f, s.ClusterProject(c)) {
		t.Fatalf("cluster project %q missing after EnsureCluster", s.ClusterProject(c))
	}
	if err := f.ReleaseCluster(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if hasProject(t, f, s.ClusterProject(c)) {
		t.Errorf("cluster project %q still listed after release", s.ClusterProject(c))
	}
}

func hasProject(t *testing.T, f *Fake, name string) bool {
	t.Helper()
	projects, err := f.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

// TestEnsureAuthOnlyActsWhereTheDirectoryIsKnown pins the process split that makes registry
// authentication work at all. Writing the registry's identity configuration needs the directory
// settings, which reach the API alone - the worker deliberately never holds the bind password. So
// EnsureAuth is a hard no-op in the worker's shape and must act in the API's; folding it back into
// EnsurePlatform (leader-elected, worker-side) means nothing ever configures it and the registry sits
// on its own default forever.
func TestEnsureAuthOnlyActsWhereTheDirectoryIsKnown(t *testing.T) {
	// The worker's shape: told nothing about how this deployment authenticates.
	worker := &Fake{Settings: Settings{ManageAuth: false, AuthMode: AuthLocal}}
	if err := worker.EnsureAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if worker.AuthConfigured {
		t.Error("the worker configured the registry's authentication; it has no directory settings to do it from")
	}

	// The API's shape.
	api := &Fake{Settings: Settings{ManageAuth: true, AuthMode: AuthLDAP}}
	if err := api.EnsureAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !api.AuthConfigured {
		t.Error("the API did not configure the registry's authentication, so it would stay on its own default")
	}
}
