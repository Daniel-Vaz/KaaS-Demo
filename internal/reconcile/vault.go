package reconcile

// Vault wiring - the per-cluster and singleton halves of the HashiCorp Vault integration, shaped
// exactly like the DNS wiring (reconcileDNSWiring/releaseDNS) and the leader-elected sweeps
// (GC/metrics/health). See internal/vault for the model.
//
//   - reconcileVaultWiring provisions the cluster's KV path + read/write policies + the External
//     Secrets auth role, and applies the in-cluster ClusterSecretStore. Gated by Cluster.VaultWired,
//     run right after reconcileDNSWiring in the same bring-up / update pass.
//   - releaseVault tears the path down BEFORE the infrastructure is destroyed (like releaseDNS): a
//     cluster's secrets are its own, and a leftover path would outlive the cluster that owned it.
//   - SyncVaultAccess converges the per-user/-group access bindings under the leader lease, because
//     membership edits happen API-side and never bump a cluster's generation.

import (
	"context"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault"
)

// externalSecretsAddon is the bundled add-on that syncs Vault secrets into Kubernetes Secrets. The
// Vault wiring gates on it: a cluster that deselected it gets no Vault path (the reservation would be
// inert), and one that selected it but hasn't finished installing holds the wiring back so the
// ClusterSecretStore half is never skipped - the same shape as gatewayWiringReady + cert-manager.
const externalSecretsAddon = "external-secrets"

// externalSecretsServiceAccount is the ESO controller's ServiceAccount ("<namespace>/<name>"), the
// subject Vault's per-cluster JWT auth role is bound to. It mirrors the add-on's catalog namespace
// and the chart's default SA name.
const externalSecretsServiceAccount = "externalsecrets-system/external-secrets"

// vaultWiringReady reports whether the cluster is ready for its Vault path to be provisioned: the
// external-secrets add-on is installed. A selected-but-not-yet-installed add-on holds the wiring back
// (so the ClusterSecretStore half isn't latched away), exactly like cert-manager gates the gateway.
func vaultWiringReady(c *domain.Cluster) bool {
	return addonInstalled(c, externalSecretsAddon)
}

// reconcileVaultWiring provisions a cluster's Vault path + policies + External-Secrets auth role and
// applies its ClusterSecretStore, once external-secrets is installed. Idempotent, so the VaultWired
// marker only skips the (not-free) provisioning once done; nothing clears it - the path lives in
// Vault, which no cluster operation touches. It is dropped on delete by releaseVault.
func (r *Reconciler) reconcileVaultWiring(ctx context.Context, c *domain.Cluster) error {
	if r.Vault == nil || c.VaultWired || !vaultWiringReady(c) {
		return nil
	}
	// The material Vault needs to trust the cluster's ESO (its token issuer + public signing keys), so
	// it validates the ESO ServiceAccount token offline. Empty in fake mode, which makes EnsureCluster
	// skip the JWT auth wiring and just create the path + policies.
	eso, err := r.esoAuth(ctx, c)
	if err != nil {
		return err
	}
	if err := r.Vault.EnsureCluster(ctx, c, eso); err != nil {
		return err
	}
	r.emit(c.ID, "info", "vault", "provisioned the cluster's Vault path and read/write policies")
	// The in-cluster half: a ClusterSecretStore pointing ESO at the central Vault over the per-cluster
	// auth role (see the external_secrets ansible role).
	if err := r.Cfg.EnsureExternalSecrets(ctx, c); err != nil {
		return err
	}
	r.emit(c.ID, "info", "ansible", "wired the External Secrets Operator to Vault")
	c.VaultWired = true
	return nil
}

// esoAuth reads the cluster's service-account token issuer and public signing keys, so Vault can
// validate the ESO token offline. It leans on the config seam (the only thing that runs kubectl
// against the cluster); the Fake returns empty, and an empty result makes EnsureCluster skip the JWT
// auth role - so `make up-fake` provisions the path with no cluster to read keys from.
func (r *Reconciler) esoAuth(ctx context.Context, c *domain.Cluster) (vault.ESOAuth, error) {
	if r.Cfg == nil {
		return vault.ESOAuth{}, nil
	}
	issuer, keys, err := r.Cfg.ClusterOIDC(ctx, c)
	if err != nil {
		return vault.ESOAuth{}, err
	}
	if len(keys) == 0 {
		return vault.ESOAuth{}, nil
	}
	return vault.ESOAuth{Issuer: issuer, PublicKeys: keys, ServiceAcct: externalSecretsServiceAccount}, nil
}

// releaseVault removes the cluster's Vault path, policies and auth role. It runs in PhaseDeleting
// BEFORE the infrastructure is destroyed (see releaseDNS for the same ordering rule): a cluster's
// secrets are its own, and a path left standing would outlive the cluster that owned it. Idempotent -
// removing an absent path succeeds - so it is safe on every deleting tick and on a retry.
func (r *Reconciler) releaseVault(ctx context.Context, c *domain.Cluster) error {
	if r.Vault == nil {
		return nil
	}
	if err := r.Vault.ReleaseCluster(ctx, c); err != nil {
		return err
	}
	if c.VaultWired {
		r.emit(c.ID, "info", "vault", "released the cluster's Vault path and policies")
	}
	c.VaultWired = false
	return nil
}

// EnsureVaultPlatform provisions the platform-wide Vault objects (the KV mount, the auth backend
// mirroring the portal, the admin policy + group). Idempotent; called once at leader startup and
// self-healing on a Vault restart. A failure is logged, not fatal - a Vault outage must not stop the
// reconciler, and the next sweep retries.
func (r *Reconciler) EnsureVaultPlatform(ctx context.Context) {
	if r.Vault == nil {
		return
	}
	if err := r.Vault.EnsurePlatform(ctx); err != nil {
		r.Log.Error("vault: ensure platform", "err", err)
	}
}

// SyncVaultAccess converges Vault's per-user/-group access bindings to the platform's current state.
// It runs under the leader lease on a ticker (like GC/metrics/health) because a membership or group
// edit is an API-side write that never bumps a cluster's generation, so the per-cluster reconcile
// loop would never see it. Non-destructive: it only (re)writes identity entities/groups and policy
// attachments - a cluster's secret data is only ever deleted by releaseVault.
func (r *Reconciler) SyncVaultAccess(ctx context.Context) {
	if r.Vault == nil {
		return
	}
	users, err := r.Store.ListUsers()
	if err != nil {
		r.Log.Error("vault: list users", "err", err)
		return
	}
	groups, err := r.Store.ListGroups()
	if err != nil {
		r.Log.Error("vault: list groups", "err", err)
		return
	}
	clusters, err := r.Store.ListClusters()
	if err != nil {
		r.Log.Error("vault: list clusters", "err", err)
		return
	}
	live := clusters[:0:0]
	for _, c := range clusters {
		if c.Phase != domain.PhaseDeleted {
			live = append(live, c)
		}
	}
	if err := r.Vault.SyncAccess(ctx, vault.AccessSnapshot{Users: users, Groups: groups, Clusters: live}); err != nil {
		r.Log.Error("vault: sync access", "err", err)
	}
}
