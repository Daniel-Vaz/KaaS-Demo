package values

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
)

// helmShowTimeout bounds a single `helm show values` fetch (it hits the chart repo over the network)
// so a slow/offline repo falls back to the synthesized doc instead of wedging the editor request.
const helmShowTimeout = 30 * time.Second

// Provider fetches a chart's own default values.yaml - the un-overridden baseline the editor merges
// the catalog overrides onto. It does NOT apply the catalog overrides (Merged does that).
//
// The seam mirrors the others in internal/app: a Fake for demos/tests (no helm, no network) and a
// real Helm implementation, selected by KAAS_ADDON_VALUES. Unlike the kubectl-proxied seams this
// one needs no cluster - `helm show values` only reads the chart repo - so it runs API-side.
// Production would proxy it through the worker like the other seams, and mint a scoped token, but a
// chart-values lookup touches no cluster state, so API-side is a fair shortcut here.
type Provider interface {
	// Defaults returns the chart's full values.yaml for this catalog add-on, as a YAML document.
	Defaults(ctx context.Context, entry catalog.Addon) (string, error)
}

// Fake synthesizes a plausible values document from the catalog, so the demo shows a real editor
// without helm or network access. It expands the catalog's curated `--set` overrides into nested
// YAML under a header that flags the doc as synthesized.
type Fake struct{}

func NewFake() Fake { return Fake{} }

func (Fake) Defaults(_ context.Context, entry catalog.Addon) (string, error) {
	// Present the curated overrides as the "defaults" body - in fake mode we can't fetch the chart's
	// real values.yaml, so the platform's curated set is the most useful stand-in. Merged() then
	// overlays the same set idempotently, so the editor seed is stable.
	body, err := Merged(entry, "")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(body) == "" || strings.TrimSpace(body) == "{}" {
		body = "# This chart uses its built-in defaults; the platform sets no overrides.\n"
	}
	header := fmt.Sprintf(
		"# %s - Helm chart %q (%s)\n"+
			"# NOTE: synthesized in fake mode. The real platform fetches the chart's full\n"+
			"# values.yaml via `helm show values %s --repo %s --version %s`.\n"+
			"# The keys below are the platform-curated overrides expanded into YAML.\n",
		entry.Name, entry.Chart, entry.Version, entry.Chart, entry.Repo, entry.Version)
	return header + body, nil
}

// Helm is the real Provider: it shells to `helm show values` to fetch the chart's own values.yaml.
// The repo is passed inline via --repo, so no `helm repo add` state is needed (same convention as
// the helm addons manager).
type Helm struct {
	Bin string // "helm"
}

func NewHelm(bin string) Helm {
	if bin == "" {
		bin = "helm"
	}
	return Helm{Bin: bin}
}

func (h Helm) Defaults(ctx context.Context, entry catalog.Addon) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, helmShowTimeout)
	defer cancel()
	args := []string{"show", "values", entry.Chart}
	// An OCI chart (oci:// ref) carries its registry in the ref and takes no --repo; a classic HTTP
	// chart repo is passed inline via --repo (same convention as the helm addons manager).
	if !strings.HasPrefix(entry.Chart, "oci://") {
		args = append(args, "--repo", entry.Repo)
	}
	args = append(args, "--version", entry.Version)
	out, err := procstream.Capture(ctx, "", os.Environ(), func(string) {}, h.Bin, args...)
	if err != nil {
		return "", fmt.Errorf("values: helm show values %q: %w", entry.Name, err)
	}
	return string(out), nil
}

// Fallback tries Primary (real helm) and, if it errors (helm missing, repo unreachable, timeout),
// serves Backup (the synthesized doc) so the editor always has something to show. This is what the
// default "auto" selection uses when a helm binary is present.
type Fallback struct {
	Primary Provider
	Backup  Provider
}

func (f Fallback) Defaults(ctx context.Context, entry catalog.Addon) (string, error) {
	if s, err := f.Primary.Defaults(ctx, entry); err == nil {
		return s, nil
	}
	return f.Backup.Defaults(ctx, entry)
}
