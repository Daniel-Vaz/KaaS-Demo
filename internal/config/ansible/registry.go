package ansible

// The node-facing half of the container image registry integration (internal/registry).
//
// Two things live here, and they run at very different times:
//
//   - registryVars is injected into EVERY playbook's extra-vars (from playbook(), next to
//     ansible_ssh_common_args), so the registry_trust role runs on the bootstrap path - before
//     kubeadm, before the CNI, before any add-on. That timing is the whole point: a node's first
//     image pull happens during bring-up, and configuration applied when the cluster reaches Ready
//     would arrive after every pull it was meant to accelerate. The role is a hard no-op when the
//     vars are absent, so a deployment with no registry is untouched.
//
//   - EnsureRegistryPullSecret is post-Ready reconcile work (reconcileRegistryWiring): the cluster's
//     own private project needs a credential, and that one IS per cluster and IS secret, so it takes
//     the ordinary route - a Kubernetes imagePullSecret applied with kubectl.
//
// The split is the load-bearing part. Trust and mirrors carry no credential at all (the caches are
// public), which is what lets them be derived from configuration alone and applied by any replica at
// any time; the per-cluster push credential is sealed, minted once, and delivered separately.

import (
	"context"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// registryPullSecretName is the Secret every cluster gets, in every namespace the platform creates
// it in. A fixed name so a tenant's manifests can reference it without asking the platform what it
// is called.
const registryPullSecretName = "kaas-registry"

// registryVars renders the node-trust configuration as Ansible extra-vars. Empty when this
// deployment has no registry, which makes the role skip itself entirely.
//
// The mirror list is passed as structured data (a list of dicts) rather than pre-rendered TOML: the
// role owns the file format, so a containerd config change is one template edit rather than a Go
// string-builder change plus a role change.
func (m *Manager) registryVars() map[string]any {
	t := m.cfg.Registry
	if !t.Configured() {
		return nil
	}
	mirrors := make([]map[string]any, 0, len(t.Mirrors))
	for _, mi := range t.Mirrors {
		mirrors = append(mirrors, map[string]any{
			"host":   mi.Host,
			"server": mi.Server,
			"mirror": mi.Mirror,
		})
	}
	return map[string]any{
		"registry_host":     t.Host,
		"registry_ca_pem":   t.CAPEM,
		"registry_insecure": t.Insecure,
		"registry_mirrors":  mirrors,
	}
}

// EnsureRegistryPullSecret applies the cluster's dockerconfigjson Secret so workloads can pull from
// (and CI can push to) the cluster's own private project. Idempotent: the role generates the Secret
// and `kubectl apply`s it, so a re-run with the same credential is a no-op and a re-run with a
// rotated one converges.
//
// It is created in the `default` namespace only. Copying it into every namespace would mean watching
// namespaces - a controller the platform does not have - and the honest alternative is already
// available to tenants: the same credential is written into the cluster's Vault path, where the
// bundled External Secrets Operator can sync it wherever it is wanted.
func (m *Manager) EnsureRegistryPullSecret(ctx context.Context, c *domain.Cluster, username, secret string) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	if !m.cfg.Registry.Configured() || username == "" {
		return nil
	}
	return m.playbook(ctx, c, "registry-pull-secret.yml", map[string]any{
		"registry_secret_name":      registryPullSecretName,
		"registry_robot_username":   username,
		"registry_robot_secret":     secret,
		"registry_secret_namespace": "default",
	})
}
