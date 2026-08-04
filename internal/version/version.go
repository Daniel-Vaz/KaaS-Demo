// Package version carries the build identity of whatever binary embeds it: which release this is,
// which commit it was built from, and when. Everything here is a var, not a const, because the
// values are injected at link time with `-ldflags -X` (see the Makefile's LDFLAGS and the
// deploy/Containerfile.* ARGs) - a `go build ./...` with no flags leaves the defaults in place and
// still produces a working binary.
//
// The defaults deliberately read as "this is not a release". An unstamped build says `dev`, which is
// exactly what a developer running `go run ./cmd/api` should see; a released image says `1.4.0`. The
// API surfaces this on GET /version and the portal shows it, so "what is this deployment running?"
// is answerable from the browser without shelling into anything. See docs/deploy/releasing.md.
package version

import "runtime/debug"

// Injected at build time via -ldflags. See Makefile (LDFLAGS) and deploy/Containerfile.*.
var (
	// Version is the platform version - the `X.Y.Z` of the `vX.Y.Z` tag that built this artifact.
	Version = "dev"
	// Commit is the full git SHA the artifact was built from.
	Commit = "unknown"
	// Date is the build timestamp, RFC 3339 in UTC.
	Date = "unknown"
)

// Info is the shape served by the API's GET /version.
//
// It carries the three facts above and nothing else. That is a deliberate limit rather than an
// oversight: /version is a PUBLIC route (like /healthz - see internal/api.isPublic), so it must not
// leak the build host, the source path, the toolchain or anything else that helps someone map the
// deployment. Which release is running is not a secret; the machine that built it might be.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Get returns the build identity.
//
// When the linker flags were not applied - `go run`, `go test`, `go install` from source - it falls
// back to the VCS stamp the Go toolchain embeds on its own, so even an unstamped local build can
// usually name its commit. It never invents a Version: an unstamped build is `dev`, and calling it
// anything else would make an untagged binary indistinguishable from a release.
func Get() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date}
	if info.Commit != "unknown" && info.Date != "unknown" {
		return info
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "unknown" && s.Value != "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == "unknown" && s.Value != "" {
				info.Date = s.Value
			}
		}
	}
	return info
}

// String is the one-line form used in logs at start-up: "1.4.0 (a1b2c3d, 2026-08-04T10:00:00Z)".
func String() string {
	i := Get()
	c := i.Commit
	if len(c) > 7 {
		c = c[:7]
	}
	return i.Version + " (" + c + ", " + i.Date + ")"
}
