package app

// The Secrets page (the portal's ConfigMaps and Secrets tabs) and the "View in Vault" handoff.
//
// ConfigMaps are read with the actor's OWN per-user reader credential (workloadRead) - the built-in
// `view` role covers ConfigMaps, so the read is attributed to the user and a read-role member sees
// exactly what they may. Secrets are different: the `view` role deliberately cannot read Secret data,
// so the platform reads them with the cluster ADMIN kubeconfig server-side (like Monitoring/Audit) and
// returns only REDACTED metadata - key names and byte lengths, never a value. So a read-role group-mate
// can browse a cluster's secrets without ever being handed one.

import (
	"context"
	"errors"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault"
)

// --- ConfigMaps (per-user reader credential; view covers ConfigMaps) ---

// ConfigMaps lists the cluster's ConfigMaps (namespace == "" means all namespaces).
func (a *App) ConfigMaps(ctx context.Context, actor *domain.User, id, namespace string) ([]kube.ConfigMapSummary, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.ConfigMaps(ctx, c, kc, namespace)
}

// ConfigMap returns one ConfigMap's detail (its full data - ConfigMaps are not secret).
func (a *App) ConfigMap(ctx context.Context, actor *domain.User, id string, ref kube.ConfigMapRef) (*kube.ConfigMapDetail, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.ConfigMap(ctx, c, kc, ref)
}

// ConfigMapManifest returns a ConfigMap's YAML.
func (a *App) ConfigMapManifest(ctx context.Context, actor *domain.User, id string, ref kube.ConfigMapRef) (string, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return "", err
	}
	return a.Kube.ConfigMapManifest(ctx, c, kc, ref)
}

// --- Secrets (admin kubeconfig server-side; redacted metadata only) ---

// secretRead resolves a cluster for a read-only Secret query: view access (owner, admin, or any
// group-mate), the cluster must be Ready, and it returns the cluster ADMIN kubeconfig - the `view`
// role can't read Secrets, so the platform reads them itself and returns only redacted metadata.
func (a *App) secretRead(actor *domain.User, id string) (*domain.Cluster, []byte, error) {
	c, err := a.authorizeCluster(actor, id)
	if err != nil {
		return nil, nil, err
	}
	if c.Phase != domain.PhaseReady {
		return nil, nil, ErrClusterNotReady
	}
	kc, err := a.openSecret(id, domain.SecretKubeconfig)
	if err != nil {
		return nil, nil, err
	}
	return c, kc, nil
}

// Secrets lists the cluster's Secrets as summary rows (keys only, never values).
func (a *App) ClusterSecrets(ctx context.Context, actor *domain.User, id, namespace string) ([]kube.SecretSummary, error) {
	c, kc, err := a.secretRead(actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Secrets(ctx, c, kc, namespace)
}

// Secret returns one Secret's detail with every value REDACTED (keys + byte lengths only).
func (a *App) ClusterSecret(ctx context.Context, actor *domain.User, id string, ref kube.SecretRef) (*kube.SecretDetail, error) {
	c, kc, err := a.secretRead(actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Secret(ctx, c, kc, ref)
}

// SecretManifest returns a Secret's YAML with the data values scrubbed to a placeholder.
func (a *App) ClusterSecretManifest(ctx context.Context, actor *domain.User, id string, ref kube.SecretRef) (string, error) {
	c, kc, err := a.secretRead(actor, id)
	if err != nil {
		return "", err
	}
	return a.Kube.SecretManifest(ctx, c, kc, ref)
}

// --- "View in Vault" handoff ---

// ErrVaultNotWired is returned when a Ready cluster has no Vault path yet (its external-secrets add-on
// isn't installed, or the wiring hasn't run). The API maps it to 409, like ErrClusterNotReady.
var ErrVaultNotWired = errors.New("this cluster's Vault path is not provisioned yet (needs the external-secrets add-on)")

// VaultSession is the "View in Vault" handoff: a short-lived Vault token scoped to the actor's access
// on THIS cluster, plus the Vault UI URL for its path. The portal opens the URL and hands the token to
// Vault, so the user lands on the cluster's KV subtree already signed in.
type VaultSession struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// VaultSession mints the handoff. View-scoped: any group-mate who can see the cluster can browse its
// Vault path, and Vault itself enforces read vs. write (the token carries the read policy for a
// read-role member, the write policy for a writer/owner, the admin policy for a platform admin) - the
// same read/write split the portal resolves, mirrored into Vault. Requires the cluster to be Ready and
// its Vault path provisioned (VaultWired).
func (a *App) VaultSession(ctx context.Context, actor *domain.User, id string) (*VaultSession, error) {
	c, err := a.authorizeCluster(actor, id)
	if err != nil {
		return nil, err
	}
	if c.Phase != domain.PhaseReady {
		return nil, ErrClusterNotReady
	}
	if !c.VaultWired {
		return nil, ErrVaultNotWired
	}
	var policy string
	switch {
	case actor.IsAdmin:
		policy = vault.PolicyAdmin
	case a.accessTo(actor, c) == accessFull:
		policy = vault.PolicyWrite(c.ID)
	default:
		policy = vault.PolicyRead(c.ID)
	}
	token, err := a.Vault.MintUserToken(ctx, []string{policy}, map[string]string{
		"username": actor.Username,
		"cluster":  c.ID,
	})
	if err != nil {
		return nil, err
	}
	return &VaultSession{URL: a.vaultSettings.UIPath(c.ID), Token: token}, nil
}
