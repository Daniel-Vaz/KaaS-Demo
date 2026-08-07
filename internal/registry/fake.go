package registry

// The Fake registry seam: no registry I/O at all. It records what it would have provisioned and
// synthesizes a plausible, deterministic view for the portal, so `make up-fake`, every unit test and
// the browser demo (cmd/demo-wasm) exercise the whole flow - admission, wiring, the Registry page,
// the push instructions - with no Harbor anywhere.
//
// The synthesized reads are derived from control-plane state (the cluster's own add-ons) rather than
// invented, for the same reason the other fakes are: a demo that shows a fleet the platform actually
// built is worth more than a fixture, and it makes the page's grouping and filtering exercisable.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Fake implements both Manager and Querier.
type Fake struct {
	Log      *slog.Logger
	Settings Settings

	mu sync.Mutex
	// Platform records that EnsurePlatform ran.
	Platform bool
	// AuthConfigured records that EnsureAuth ran AND was allowed to act (Settings.ManageAuth).
	AuthConfigured bool
	// Ensured is the set of project names that exist (a released cluster's is removed).
	Ensured map[string]bool
	// Robots is the per-cluster minted credential, keyed by cluster id.
	Robots map[string]RobotCredential
	// LastDesired is the most recent access convergence, for tests.
	LastDesired DesiredState
	// PasswordSets counts SetUserPassword calls, for tests.
	PasswordSets int
	// clusters remembers the cluster behind each project so the synthesized listing is keyed on
	// something real.
	clusters map[string]*domain.Cluster
}

// NewFake returns a Fake registry seam.
func NewFake(log *slog.Logger, s Settings) *Fake {
	return &Fake{
		Log:      log,
		Settings: s,
		Ensured:  map[string]bool{},
		Robots:   map[string]RobotCredential{},
		clusters: map[string]*domain.Cluster{},
	}
}

func (f *Fake) logf(msg string, args ...any) {
	if f.Log != nil {
		f.Log.Info(msg, args...)
	}
}

// --- Manager ---------------------------------------------------------------------------------

func (f *Fake) EnsurePlatform(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Platform = true
	f.Ensured[f.Settings.Library()] = true
	for _, u := range f.Settings.Upstreams {
		f.Ensured[f.Settings.CacheProject(u)] = true
	}
	f.logf("registry(fake): ensured the library and proxy-cache projects", "mirror", f.Settings.Mirror)
	return nil
}

// EnsureAuth records that the auth backend was configured, and by which process - the split that
// matters is that only one holding the directory settings ever does (see Manager.EnsureAuth).
func (f *Fake) EnsureAuth(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Settings.ManageAuth {
		f.logf("registry(fake): leaving the auth backend untouched (no directory config in this process)")
		return nil
	}
	f.AuthConfigured = true
	f.logf("registry(fake): configured the auth backend", "mode", f.Settings.AuthMode)
	return nil
}

func (f *Fake) EnsureCluster(_ context.Context, c *domain.Cluster) (RobotCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	project := f.Settings.ClusterProject(c)
	f.Ensured[project] = true
	f.clusters[project] = c
	if r, ok := f.Robots[c.ID]; ok {
		// Already minted: return the identity with NO secret, exactly as the real implementation
		// does, so a re-run of the wiring can never rotate a credential the cluster already uses.
		return RobotCredential{Username: r.Username, Expires: r.Expires}, nil
	}
	cred := RobotCredential{
		Username: "robot$" + ClusterRobot(c),
		Secret:   "fake-robot-secret-" + c.ID,
		Expires:  time.Now().Add(f.Settings.RobotTTL),
	}
	f.Robots[c.ID] = cred
	f.logf("registry(fake): ensured cluster project", "cluster", c.Name, "project", project)
	return cred, nil
}

func (f *Fake) ReleaseCluster(_ context.Context, c *domain.Cluster) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	project := f.Settings.ClusterProject(c)
	delete(f.Ensured, project)
	delete(f.clusters, project)
	delete(f.Robots, c.ID)
	f.logf("registry(fake): released cluster project", "cluster", c.Name, "project", project)
	return nil
}

func (f *Fake) SyncAccess(_ context.Context, snap AccessSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LastDesired = DesiredAccess(snap, f.Settings)
	// Keep the synthesized listing in step with the live fleet, so a cluster created in the demo
	// shows a project without waiting for its wiring step.
	for _, c := range snap.Clusters {
		if c.RegistryWired {
			f.Ensured[f.Settings.ClusterProject(c)] = true
			f.clusters[f.Settings.ClusterProject(c)] = c
		}
	}
	f.logf("registry(fake): synced access",
		"users", len(f.LastDesired.Users), "members", len(f.LastDesired.Members))
	return nil
}

func (f *Fake) SetUserPassword(_ context.Context, username, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PasswordSets++
	f.logf("registry(fake): set registry password", "user", username)
	return nil
}

// --- Querier ---------------------------------------------------------------------------------

func (f *Fake) Status(context.Context) Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := Status{
		Configured:     true,
		Healthy:        true,
		Version:        "v2.15.1 (simulated)",
		Host:           f.Settings.Host,
		UIURL:          f.Settings.UIURL,
		AuthMode:       f.Settings.AuthMode,
		Mirror:         f.Settings.Mirror,
		CanSetPassword: f.Settings.CanSetPasswords(),
	}
	for _, u := range f.Settings.Upstreams {
		st.Upstreams = append(st.Upstreams, u.Host)
	}
	return st
}

func (f *Fake) Projects(context.Context) ([]Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The platform's own projects are derived from the SETTINGS, not from whether EnsurePlatform has
	// run. In fake mode nothing calls it - the platform sweeps are leader-elected and there is no
	// leader without Postgres - so keying the listing on it would hide the pull-through caches, which
	// are the most visible half of the feature, from `make up-fake` and the browser demo. A cluster's
	// project still comes from Ensured, because that one genuinely tracks per-cluster lifecycle.
	names := map[string]bool{f.Settings.Library(): true}
	if f.Settings.Mirror {
		for _, u := range f.Settings.Upstreams {
			names[f.Settings.CacheProject(u)] = true
		}
	}
	for name := range f.Ensured {
		names[name] = true
	}
	var out []Project
	for name := range names {
		p := Project{Name: name, Kind: KindExternal}
		switch {
		case name == f.Settings.Library():
			p.Kind, p.Public = KindLibrary, true
		case strings.HasPrefix(name, f.Settings.prefix()+"cache-"):
			p.Kind, p.Public = KindCache, true
			for _, u := range f.Settings.Upstreams {
				if f.Settings.CacheProject(u) == name {
					p.Upstream = u.Host
				}
			}
		default:
			p.Kind = KindCluster
		}
		repos := f.reposFor(name)
		p.RepoCount = len(repos)
		for _, r := range repos {
			p.SizeBytes += int64(r.ArtifactCount) * fakeArtifactSize(r.FullName)
		}
		if c := f.clusters[name]; c != nil {
			p.UpdatedAt = c.CreatedAt
		} else {
			p.UpdatedAt = time.Now().Add(-3 * time.Hour)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *Fake) Repositories(_ context.Context, project string) ([]Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reposFor(project), nil
}

func (f *Fake) Artifacts(_ context.Context, project, repo string) ([]Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	full := project + "/" + repo
	n := 1 + int(fakeHash(full)%3)
	var out []Artifact
	for i := range n {
		tag := fmt.Sprintf("v1.%d.%d", n, i)
		if i == 0 {
			tag = "latest"
		}
		sum := sha256.Sum256([]byte(full + tag))
		out = append(out, Artifact{
			Digest:    "sha256:" + hex.EncodeToString(sum[:])[:32],
			Tags:      []string{tag},
			SizeBytes: fakeArtifactSize(full) + int64(i)*1_500_000,
			PushedAt:  time.Now().Add(-time.Duration(i*7+2) * time.Hour),
			Type:      "IMAGE",
		})
	}
	return out, nil
}

// reposFor synthesizes a project's repository list. A cache project mirrors what the cluster's own
// add-ons would have pulled through it - which is the whole point of the mirror, so the demo shows
// it. Caller holds the lock.
func (f *Fake) reposFor(project string) []Repository {
	var names []string
	switch {
	case project == f.Settings.Library():
		names = []string{"pause", "kaas-tools"}
	case strings.HasPrefix(project, f.Settings.prefix()+"cache-"):
		names = fakeCacheRepos(project)
	default:
		c := f.clusters[project]
		if c == nil {
			return nil
		}
		names = []string{"api", "web"}
		if len(c.Addons) > 2 {
			names = append(names, "worker")
		}
	}
	out := make([]Repository, 0, len(names))
	for _, n := range names {
		full := project + "/" + n
		out = append(out, Repository{
			Name:          n,
			FullName:      full,
			ArtifactCount: 1 + int(fakeHash(full)%3),
			PullCount:     int64(fakeHash(full) % 400),
			UpdatedAt:     time.Now().Add(-time.Duration(fakeHash(full)%72) * time.Hour),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// fakeCacheRepos names the images a cluster's add-ons really do pull from each upstream, so the
// simulated cache reads like the real one.
func fakeCacheRepos(project string) []string {
	switch {
	case strings.HasSuffix(project, "cache-dockerhub"):
		return []string{"grafana/grafana", "longhornio/longhorn-manager", "envoyproxy/envoy"}
	case strings.HasSuffix(project, "cache-ghcr"):
		return []string{"aquasecurity/trivy-operator", "external-secrets/external-secrets"}
	case strings.HasSuffix(project, "cache-quay"):
		return []string{"cilium/cilium", "cilium/operator-generic", "prometheus/prometheus"}
	case strings.HasSuffix(project, "cache-k8s"):
		return []string{"metrics-server/metrics-server", "sig-storage/csi-provisioner", "pause"}
	}
	return nil
}

// fakeHash is a small stable hash, so every synthesized number is the same on every replica and on
// every reload - a demo whose figures jitter reads as broken.
func fakeHash(s string) uint32 {
	var h uint32 = 2166136261
	for i := range len(s) {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func fakeArtifactSize(full string) int64 {
	return 20_000_000 + int64(fakeHash(full)%180)*1_000_000
}
