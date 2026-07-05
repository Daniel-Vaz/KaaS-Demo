package kubectl

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
)

// The kube.ConfigReader half of the real Client: ConfigMaps and Secrets read with the same Execer,
// arg-building and JSON shapes as the workload half (see kubectl.go). Secret VALUES never leave this
// package: summaries and details carry only keys and byte lengths, and SecretManifest scrubs the data
// map before returning the YAML.

// rawMetaCfg is the object metadata this file needs. Named to avoid colliding with the raw structs in
// storage.go/network.go.
type rawMetaCfg struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"ownerReferences"`
}

type rawConfigMap struct {
	Metadata   rawMetaCfg        `json:"metadata"`
	Immutable  *bool             `json:"immutable"`
	Data       map[string]string `json:"data"`
	BinaryData map[string]string `json:"binaryData"`
}

type rawSecret struct {
	Metadata  rawMetaCfg        `json:"metadata"`
	Immutable *bool             `json:"immutable"`
	Type      string            `json:"type"`
	Data      map[string]string `json:"data"` // base64-encoded values - never returned to the caller
}

func (c *Client) ConfigMaps(ctx context.Context, cl *domain.Cluster, kc []byte, namespace string) ([]kube.ConfigMapSummary, error) {
	out, err := c.run(ctx, kc, cl.ID, getArgs("configmaps", namespace)...)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawConfigMap `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode configmaps: %w", err)
	}
	res := make([]kube.ConfigMapSummary, 0, len(list.Items))
	for _, it := range list.Items {
		res = append(res, it.summary())
	}
	return res, nil
}

func (c *Client) ConfigMap(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.ConfigMapRef) (*kube.ConfigMapDetail, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "configmaps", ref.Name, "-n", ref.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var obj rawConfigMap
	if err := json.Unmarshal(out, &obj); err != nil {
		return nil, fmt.Errorf("decode configmap: %w", err)
	}
	d := kube.ConfigMapDetail{
		ConfigMapSummary: obj.summary(),
		Labels:           obj.Metadata.Labels,
		Annotations:      obj.Metadata.Annotations,
		Data:             obj.Data,
		BinaryKeys:       sortedKeys(obj.BinaryData),
	}
	return &d, nil
}

func (c *Client) ConfigMapManifest(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.ConfigMapRef) (string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "configmaps", ref.Name, "-n", ref.Namespace, "-o", "yaml")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *Client) Secrets(ctx context.Context, cl *domain.Cluster, kc []byte, namespace string) ([]kube.SecretSummary, error) {
	out, err := c.run(ctx, kc, cl.ID, getArgs("secrets", namespace)...)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawSecret `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode secrets: %w", err)
	}
	res := make([]kube.SecretSummary, 0, len(list.Items))
	for _, it := range list.Items {
		res = append(res, it.summary())
	}
	return res, nil
}

func (c *Client) Secret(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.SecretRef) (*kube.SecretDetail, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "secrets", ref.Name, "-n", ref.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var obj rawSecret
	if err := json.Unmarshal(out, &obj); err != nil {
		return nil, fmt.Errorf("decode secret: %w", err)
	}
	// REDACTION: only the key names and each value's decoded byte length leave this function - never
	// the value itself.
	info := make([]kube.SecretKeyInfo, 0, len(obj.Data))
	for _, k := range sortedKeys(obj.Data) {
		info = append(info, kube.SecretKeyInfo{Key: k, Bytes: decodedLen(obj.Data[k])})
	}
	d := kube.SecretDetail{
		SecretSummary: obj.summary(),
		Labels:        obj.Metadata.Labels,
		Annotations:   obj.Metadata.Annotations,
		Keys:          info,
	}
	return &d, nil
}

// SecretManifest returns the Secret's YAML with every data/stringData value scrubbed. It reads JSON,
// replaces the values, then re-encodes as YAML, so no base64-encoded secret value is ever in the
// output.
func (c *Client) SecretManifest(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.SecretRef) (string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "secrets", ref.Name, "-n", ref.Namespace, "-o", "json")
	if err != nil {
		return "", err
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	scrub(obj, "data")
	scrub(obj, "stringData")
	y, err := yaml.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("encode secret yaml: %w", err)
	}
	return string(y), nil
}

// scrub replaces every value under the named top-level key with the redaction placeholder.
func scrub(obj map[string]any, key string) {
	m, ok := obj[key].(map[string]any)
	if !ok {
		return
	}
	for k := range m {
		m[k] = kube.RedactedValue
	}
}

// getArgs builds a `get <resource> [-n ns|--all-namespaces] -o json` argument list.
func getArgs(resource, namespace string) []string {
	args := []string{"get", resource}
	if namespace == "" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "-n", namespace)
	}
	return append(args, "-o", "json")
}

func (r rawConfigMap) summary() kube.ConfigMapSummary {
	keys := append(sortedKeys(r.Data), sortedKeys(r.BinaryData)...)
	sort.Strings(keys)
	return kube.ConfigMapSummary{
		Namespace: r.Metadata.Namespace,
		Name:      r.Metadata.Name,
		Keys:      keys,
		DataCount: len(r.Data) + len(r.BinaryData),
		Immutable: r.Immutable != nil && *r.Immutable,
		CreatedAt: r.Metadata.CreationTimestamp,
	}
}

func (r rawSecret) summary() kube.SecretSummary {
	return kube.SecretSummary{
		Namespace: r.Metadata.Namespace,
		Name:      r.Metadata.Name,
		Type:      r.Type,
		Keys:      sortedKeys(r.Data),
		DataCount: len(r.Data),
		Immutable: r.Immutable != nil && *r.Immutable,
		ManagedBy: externalSecretOwner(r.Metadata),
		CreatedAt: r.Metadata.CreationTimestamp,
	}
}

// externalSecretOwner returns the name of the ExternalSecret that owns this object (so the page can
// badge a Vault-synced Secret), or "" when it was not created by the External Secrets Operator.
func externalSecretOwner(m rawMetaCfg) string {
	for _, o := range m.OwnerReferences {
		if o.Kind == "ExternalSecret" {
			return o.Name
		}
	}
	return ""
}

// decodedLen returns the byte length of a base64-encoded Secret value (Kubernetes stores Secret data
// base64-encoded). A value that fails to decode falls back to its raw length rather than erroring -
// the length is cosmetic and must never make a Secret unviewable.
func decodedLen(b64 string) int {
	if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
		return len(raw)
	}
	return len(b64)
}
