package kube

import (
	"context"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// ConfigMap and Secret types for the portal's Secrets page (the ConfigMaps and Secrets tabs). They
// live in the kube seam rather than a seam of their own, exactly like storage.go and network.go:
// ConfigMaps and Secrets are core Kubernetes objects read with the same `kubectl get -o json` against
// the same cluster over the same worker exec agent, so the same Client interface, transport and
// view-scoped auth cover them.
//
// SECRET VALUES ARE REDACTED SERVER-SIDE. A Secret detail carries only its keys and each value's byte
// length - never the decoded bytes - and SecretManifest scrubs the data map before returning the YAML.
// The redaction happens here, above the API and above the worker↔API hop, so no plaintext secret ever
// leaves the exec agent for the browser. This mirrors the platform's stance everywhere else (the view
// RBAC can't read Secret data, Trivy redacts matched secret values, an etcd snapshot is sealed before
// it leaves the worker). ConfigMap values are NOT secret and are returned in full.

// RedactedValue is what a Secret value is replaced with wherever one would otherwise appear (the
// scrubbed manifest). The detail view shows key + byte length instead.
const RedactedValue = "<redacted>"

// ConfigMapRef / SecretRef identify one namespaced object within a cluster.
type ConfigMapRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type SecretRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ConfigMapSummary is one row on the ConfigMaps tab. Keys lists the data + binaryData key names so a
// glance says what a ConfigMap carries; DataCount is their total for the list's "N keys" column.
type ConfigMapSummary struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Keys      []string  `json:"keys"`
	DataCount int       `json:"data_count"`
	Immutable bool      `json:"immutable,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ConfigMapDetail is a ConfigMap's full view. Data carries the textual values in full - a ConfigMap is
// not a secret. BinaryKeys names the binaryData entries, whose (binary) values are omitted from the
// JSON but shown as "binary" in the UI.
type ConfigMapDetail struct {
	ConfigMapSummary
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Data        map[string]string `json:"data,omitempty"`
	BinaryKeys  []string          `json:"binary_keys,omitempty"`
}

// SecretSummary is one row on the Secrets tab. Keys lists the data key names (never their values);
// Type is the Kubernetes secret type. ManagedBy names the ExternalSecret that owns this Secret when it
// was synced from Vault by the External Secrets Operator, so the page can badge Vault-backed secrets
// and point at their source - the whole reason the Secrets page and Vault sit next to each other.
type SecretSummary struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Keys      []string  `json:"keys"`
	DataCount int       `json:"data_count"`
	Immutable bool      `json:"immutable,omitempty"`
	ManagedBy string    `json:"managed_by,omitempty"` // owning ExternalSecret, "" if not ESO-synced
	CreatedAt time.Time `json:"created_at"`
}

// SecretKeyInfo is one key of a Secret with its value REDACTED - the byte length is the only thing
// about the value that is ever revealed, so the UI can say "12 bytes" without exposing the secret.
type SecretKeyInfo struct {
	Key   string `json:"key"`
	Bytes int    `json:"bytes"`
}

// SecretDetail is a Secret's full view with every value redacted (see SecretKeyInfo). There is no
// field anywhere in this type that carries a decoded secret value.
type SecretDetail struct {
	SecretSummary
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Keys        []SecretKeyInfo   `json:"key_info"`
}

// ConfigReader reads a Ready cluster's ConfigMaps and Secrets. It is part of Client (see kube.go);
// split into its own interface purely to keep the page-shaped surfaces readable. namespace == "" means
// "all namespaces". Every method is read-only and secret values are redacted, so view access suffices -
// a read-role member's per-user reader credential serves the ConfigMap methods (the built-in `view`
// role covers ConfigMaps), while the Secret methods run with the cluster ADMIN kubeconfig server-side
// (the `view` role deliberately cannot read Secrets - the platform reads them with admin and returns
// only redacted metadata, never the values). See app.configRead / app.secretRead.
type ConfigReader interface {
	// ConfigMaps lists the cluster's ConfigMaps as summary rows.
	ConfigMaps(ctx context.Context, c *domain.Cluster, kubeconfig []byte, namespace string) ([]ConfigMapSummary, error)
	// ConfigMap returns one ConfigMap's detail (its full data - ConfigMaps are not secret).
	ConfigMap(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref ConfigMapRef) (*ConfigMapDetail, error)
	// ConfigMapManifest returns a ConfigMap's YAML.
	ConfigMapManifest(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref ConfigMapRef) (string, error)
	// Secrets lists the cluster's Secrets as summary rows (keys only, never values).
	Secrets(ctx context.Context, c *domain.Cluster, kubeconfig []byte, namespace string) ([]SecretSummary, error)
	// Secret returns one Secret's detail with every value REDACTED (keys + byte lengths only).
	Secret(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref SecretRef) (*SecretDetail, error)
	// SecretManifest returns a Secret's YAML with the data/stringData values scrubbed to RedactedValue.
	SecretManifest(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref SecretRef) (string, error)
}
