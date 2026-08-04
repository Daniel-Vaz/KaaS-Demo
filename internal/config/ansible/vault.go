package ansible

// The in-cluster half of the HashiCorp Vault integration:
//
//   - EnsureExternalSecrets applies a ClusterSecretStore that points the External Secrets Operator at
//     the central Vault, authenticating over the cluster's own per-cluster JWT auth role (see the
//     external_secrets role). Modeled on EnsureDefaultGateway.
//   - ClusterOIDC reads the cluster's service-account token issuer and public signing keys, so the
//     platform can configure Vault to validate the ESO token OFFLINE. It runs `kubectl get --raw`
//     on a control plane (cluster-oidc.yml), writes the results to the artifacts dir, and converts the
//     JWKS to the PEM Vault's jwt_validation_pubkeys wants - the same run-playbook-then-read-artifact
//     shape as CertExpiry.

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault"
)

// EnsureExternalSecrets applies the cluster's ClusterSecretStore (Vault provider, jwt auth over the
// per-cluster auth mount/role, confined to the cluster's own KV subtree). Idempotent (`kubectl apply`
// on the first control plane). The Vault mount comes from the worker's env - the same place the
// worker's management token lives.
//
// The ADDRESS is the one thing that does not: this is the address ESO dials from INSIDE the cluster,
// which is not necessarily the one the worker uses. On a tunnelled deployment the worker reaches
// Vault locally (the compose network, or a published port on the container host) while the cluster
// can only reach it wherever it is exposed on the node network. Collapsing the two makes the
// reconcile loop depend on the tenant-facing route: a broken tunnel then fails reconcileVaultWiring
// and loops every cluster in InstallingAddons, rather than degrading only ESO's secret syncing.
// KAAS_VAULT_CLUSTER_ADDR splits them; unset, it falls back to the worker's own address, which is
// correct whenever both sides share one route (up-fake, an in-cluster Vault, a routable host).
func (m *Manager) EnsureExternalSecrets(ctx context.Context, c *domain.Cluster) error {
	if _, err := m.prep(c); err != nil {
		return err
	}
	mount := os.Getenv("KAAS_VAULT_MOUNT")
	if mount == "" {
		mount = vault.DefaultMount
	}
	addr := os.Getenv("KAAS_VAULT_CLUSTER_ADDR")
	if addr == "" {
		addr = os.Getenv("KAAS_VAULT_ADDR")
	}
	return m.playbook(ctx, c, "external-secrets.yml", map[string]any{
		"vault_addr":           addr,
		"vault_mount":          mount,
		"vault_cluster_prefix": vault.ClusterPrefix(c.ID),
		"vault_auth_mount":     "jwt-" + c.ID,
		"vault_auth_role":      vault.AuthRole(c.ID),
		"eso_namespace":        "externalsecrets-system",
	})
}

// ClusterOIDC runs cluster-oidc.yml (kubectl get --raw on a control plane), then reads the issuer and
// converts the JWKS to PEM public keys. An RSA-only conversion: Kubernetes signs service-account
// tokens with RSA by default, and a key it can't convert is skipped rather than failing the whole
// read (Vault simply won't validate tokens for that key - a missing key degrades, it doesn't break).
func (m *Manager) ClusterOIDC(ctx context.Context, c *domain.Cluster) (string, []string, error) {
	art, err := m.prep(c)
	if err != nil {
		return "", nil, err
	}
	if err := m.playbook(ctx, c, "cluster-oidc.yml", map[string]any{"artifacts_dir": art}); err != nil {
		return "", nil, err
	}
	issuer, err := readIssuer(filepath.Join(art, "oidc.json"))
	if err != nil {
		return "", nil, err
	}
	keys, err := jwksToPEM(filepath.Join(art, "jwks.json"))
	if err != nil {
		return "", nil, err
	}
	return issuer, keys, nil
}

func readIssuer(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("ansible: read oidc config: %w", err)
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("ansible: parse oidc config: %w", err)
	}
	return doc.Issuer, nil
}

// jwksToPEM reads a JWKS file and returns the PEM-encoded SubjectPublicKeyInfo of each RSA key.
func jwksToPEM(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ansible: read jwks: %w", err)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("ansible: parse jwks: %w", err)
	}
	var out []string
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			continue
		}
		out = append(out, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	}
	return out, nil
}
