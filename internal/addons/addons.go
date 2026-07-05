// Package addons is the add-on seam: install cluster add-ons onto a ready cluster.
//
// Add-ons are catalog-as-data: chart, repo, version, and values come
// from internal/catalog - there is no separate add-on catalog here. The real implementation
// (internal/addons/helm) installs via `helm upgrade --install` (idempotent); Fake just succeeds.
package addons

import (
	"context"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Manager installs and removes add-ons on a ready cluster, given its admin kubeconfig.
// Both operations must be idempotent (Install re-applies; Uninstall tolerates absence).
type Manager interface {
	Install(ctx context.Context, c *domain.Cluster, a domain.Addon, kubeconfig []byte) error
	Uninstall(ctx context.Context, c *domain.Cluster, a domain.Addon, kubeconfig []byte) error
}

// Extras is per-install add-on configuration the CATALOG cannot carry, because it is a property of
// this deployment rather than of the chart: where the site's DNS servers are, what credentials reach
// them, which domain THIS cluster owns. The catalog stays the single source of chart/repo/version and
// of the values that are true everywhere; a deployment-shaped value arrives here instead of turning
// catalog.json into environment config. Today only external-dns uses it (see internal/app/dns.go).
//
// Values are applied as `--set` AFTER everything else - the catalog's own values and any per-cluster
// values override - so they always win. That ordering is load-bearing: a user editing an add-on's
// values in the portal must not be able to unhook it from the platform's DNS.
type Extras struct {
	Values map[string]string // extra helm --set key=value
	// Secret, when set, is a Kubernetes Secret to create (idempotently) in the add-on's namespace
	// before the chart installs, for credentials that must not ride in Helm values. Values then
	// reference it (env[…].valueFrom.secretKeyRef), so the credential never lands in a Deployment's
	// env - where the cluster's own read-role users could read it back off the pod spec.
	Secret *SecretSpec
}

// SecretSpec is a Kubernetes Secret the add-on needs before it installs.
type SecretSpec struct {
	Name      string
	Namespace string
	Data      map[string]string // key → plaintext value (the manager base64-encodes it)
}

// ExtrasFunc supplies Extras for one add-on on one cluster. Nil (or a zero Extras) means "nothing
// to add" - the overwhelmingly common case.
type ExtrasFunc func(c *domain.Cluster, a domain.Addon) Extras

// Fake pretends every add-on installs/removes cleanly (default; tests and no-cluster runs).
type Fake struct{}

func NewFake() *Fake { return &Fake{} }

func (Fake) Install(_ context.Context, _ *domain.Cluster, _ domain.Addon, _ []byte) error {
	return nil
}

func (Fake) Uninstall(_ context.Context, _ *domain.Cluster, _ domain.Addon, _ []byte) error {
	return nil
}
