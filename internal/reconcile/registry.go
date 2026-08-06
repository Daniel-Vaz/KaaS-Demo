package reconcile

// Container image registry wiring - the per-cluster and singleton halves of the registry
// integration, shaped exactly like the Vault wiring (reconcileVaultWiring/releaseVault/
// SyncVaultAccess). See internal/registry for the model.
//
//   - reconcileRegistryWiring provisions the cluster's own project + push/pull robot, seals the
//     credential, and applies the in-cluster pull Secret. Gated by Cluster.RegistryWired, run right
//     after reconcileVaultWiring in the same bring-up / update pass.
//   - releaseRegistry drops the project BEFORE the infrastructure is destroyed (like releaseDNS and
//     releaseVault): a cluster's images are its own, and a project left standing would outlive the
//     cluster that owned it - and its name would be reclaimed by the next cluster of the same name.
//   - SyncRegistryAccess converges the per-user project memberships under the leader lease, because
//     membership edits happen API-side and never bump a cluster's generation.
//
// NOT here, and deliberately: registry TRUST and the pull-through mirrors on cluster nodes. A node's
// first image pull happens during bootstrap, long before a cluster is Ready, so trust applied at
// this point would arrive too late to be the point of having it. It rides in Ansible's own
// bootstrap path instead - see config.Manager.SetRegistryTrust and the registry_trust role.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/registry"
)

// reconcileRegistryWiring provisions a cluster's registry project + robot and applies its in-cluster
// pull Secret. Idempotent, so the RegistryWired marker only skips the (not-free) provisioning once
// done; nothing clears it - the project lives in the registry, which no cluster operation touches.
// It is dropped on delete by releaseRegistry.
//
// Unlike the Vault wiring this gates on NO add-on: a project and a pull credential are useful to
// every Ready cluster, and nothing needs to be installed in the cluster for them to work.
func (r *Reconciler) reconcileRegistryWiring(ctx context.Context, c *domain.Cluster) error {
	if r.Registry == nil || c.RegistryWired {
		return nil
	}
	cred, err := r.Registry.EnsureCluster(ctx, c)
	if err != nil {
		return err
	}
	// A credential is returned WITH a secret only the first time it is minted. On a retry after a
	// partial run the registry returns the identity alone, and the sealed copy from the first pass is
	// the only one that exists - so fall back to it rather than storing a secretless credential and
	// handing the cluster a pull Secret that cannot authenticate.
	if !cred.Valid() {
		stored, err := r.storedRegistryCredential(c)
		if err != nil {
			return fmt.Errorf("registry: robot already exists but no stored credential for it: %w", err)
		}
		cred = stored
	} else if err := r.sealRegistryCredential(c, cred); err != nil {
		return err
	}
	if !cred.Expires.IsZero() {
		exp := cred.Expires
		c.RegistryRobotNotAfter = &exp
	}
	r.emit(c.ID, "info", "registry", "provisioned the cluster's registry project and push credential")

	// The in-cluster half: a dockerconfigjson Secret the tenant's workloads reference, and which the
	// platform's own namespaces can use. Best-effort ordering aside, this is an ordinary idempotent
	// kubectl apply.
	if err := r.Cfg.EnsureRegistryPullSecret(ctx, c, cred.Username, cred.Secret); err != nil {
		return err
	}
	r.emit(c.ID, "info", "ansible", "applied the cluster's registry pull secret")
	c.RegistryWired = true
	return nil
}

// sealRegistryCredential stores the robot credential under SecretRegistryPull. It is sealed like
// every other per-cluster secret because it is a PUSH credential for the cluster's own project - the
// tenant's images, not merely the ability to pull public ones.
func (r *Reconciler) sealRegistryCredential(c *domain.Cluster, cred registry.RobotCredential) error {
	blob, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	ct, err := r.Secrets.Seal(blob)
	if err != nil {
		return err
	}
	return r.Store.SaveSecret(c.ID, domain.SecretRegistryPull, ct)
}

// storedRegistryCredential reads back the sealed robot credential.
func (r *Reconciler) storedRegistryCredential(c *domain.Cluster) (registry.RobotCredential, error) {
	pt, err := r.getSecret(c.ID, domain.SecretRegistryPull)
	if err != nil {
		return registry.RobotCredential{}, err
	}
	var cred registry.RobotCredential
	if err := json.Unmarshal(pt, &cred); err != nil {
		return registry.RobotCredential{}, err
	}
	return cred, nil
}

// releaseRegistry removes the cluster's registry project and robot. It runs in PhaseDeleting BEFORE
// the infrastructure is destroyed (see releaseDNS for the same ordering rule), and here the reason is
// sharper than "tidiness": a project is named after the cluster, and cluster names are reusable once
// a cluster is gone - so a project left behind would be silently inherited by the NEXT cluster of
// that name, handing one tenant another tenant's images. Idempotent - releasing an absent project
// succeeds - so it is safe on every deleting tick and on a retry.
func (r *Reconciler) releaseRegistry(ctx context.Context, c *domain.Cluster) error {
	if r.Registry == nil {
		return nil
	}
	if err := r.Registry.ReleaseCluster(ctx, c); err != nil {
		return err
	}
	if c.RegistryWired {
		r.emit(c.ID, "info", "registry", "released the cluster's registry project and credential")
	}
	c.RegistryWired = false
	return nil
}

// EnsureRegistryPlatform provisions the platform-wide registry objects (the auth backend mirroring
// the portal, the library and proxy-cache projects, the upstream registry endpoints, the GC
// schedule). Called once at leader startup and self-healing on a registry restart.
//
// A failure is logged, not fatal - a registry outage must not stop the reconciler, and cluster
// bring-up survives it because containerd falls back to the upstream whenever a mirror does not
// answer. Note what this does NOT do: it hands nothing to the node-trust path, because that path
// needs no credential and derives everything it uses from the settings alone (see
// registry.NodeTrust), which is what lets a non-leader replica configure a node's containerd.
func (r *Reconciler) EnsureRegistryPlatform(ctx context.Context) {
	if r.Registry == nil {
		return
	}
	if err := r.Registry.EnsurePlatform(ctx); err != nil {
		r.Log.Error("registry: ensure platform", "err", err)
	}
}

// SyncRegistryAccess converges the registry's per-user project memberships to the platform's current
// state. It runs under the leader lease on a ticker (like GC/metrics/health and the Vault sweep)
// because a membership or group edit is an API-side write that never bumps a cluster's generation,
// so the per-cluster reconcile loop would never see it.
func (r *Reconciler) SyncRegistryAccess(ctx context.Context) {
	if r.Registry == nil {
		return
	}
	users, err := r.Store.ListUsers()
	if err != nil {
		r.Log.Error("registry: list users", "err", err)
		return
	}
	groups, err := r.Store.ListGroups()
	if err != nil {
		r.Log.Error("registry: list groups", "err", err)
		return
	}
	clusters, err := r.Store.ListClusters()
	if err != nil {
		r.Log.Error("registry: list clusters", "err", err)
		return
	}
	live := clusters[:0:0]
	for _, c := range clusters {
		if c.Phase != domain.PhaseDeleted {
			live = append(live, c)
		}
	}
	if err := r.Registry.SyncAccess(ctx, registry.AccessSnapshot{Users: users, Groups: groups, Clusters: live}); err != nil {
		r.Log.Error("registry: sync access", "err", err)
	}
}
