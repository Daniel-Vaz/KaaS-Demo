package kube

import (
	"context"
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// The kube.ConfigReader half of the Fake: a plausible, deterministic set of ConfigMaps and Secrets
// synthesized from control-plane state, so the Secrets page is demoable with no cluster. Secret VALUES
// are never synthesized at all - the fake carries only key names and byte lengths, exactly what the
// real backend returns after redaction, so the two agree on "a Secret shows no values".

// fakeConfigMap / fakeSecret are the synthesized shapes. A ConfigMap keeps its (non-secret) values; a
// Secret keeps only key→byte-length, never a value.
type fakeConfigMap struct {
	namespace string
	name      string
	data      map[string]string
	immutable bool
}

type fakeSecret struct {
	namespace string
	name      string
	typ       string
	keys      []SecretKeyInfo
	managedBy string // owning ExternalSecret (Vault-synced), "" otherwise
}

func (f *Fake) ConfigMaps(_ context.Context, c *domain.Cluster, _ []byte, namespace string) ([]ConfigMapSummary, error) {
	var out []ConfigMapSummary
	for _, cm := range f.buildConfigMaps(c) {
		if namespace != "" && cm.namespace != namespace {
			continue
		}
		out = append(out, f.configMapSummary(cm))
	}
	return out, nil
}

func (f *Fake) ConfigMap(_ context.Context, c *domain.Cluster, _ []byte, ref ConfigMapRef) (*ConfigMapDetail, error) {
	for _, cm := range f.buildConfigMaps(c) {
		if cm.namespace == ref.Namespace && cm.name == ref.Name {
			d := ConfigMapDetail{
				ConfigMapSummary: f.configMapSummary(cm),
				Labels:           map[string]string{"app.kubernetes.io/managed-by": "kaas-demo"},
				Data:             cm.data,
			}
			return &d, nil
		}
	}
	return nil, fmt.Errorf("configmap %s/%s not found", ref.Namespace, ref.Name)
}

func (f *Fake) ConfigMapManifest(_ context.Context, c *domain.Cluster, _ []byte, ref ConfigMapRef) (string, error) {
	for _, cm := range f.buildConfigMaps(c) {
		if cm.namespace == ref.Namespace && cm.name == ref.Name {
			obj := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": cm.name, "namespace": cm.namespace},
				"data":       cm.data,
			}
			y, _ := yaml.Marshal(obj)
			return string(y), nil
		}
	}
	return "", fmt.Errorf("configmap %s/%s not found", ref.Namespace, ref.Name)
}

func (f *Fake) Secrets(_ context.Context, c *domain.Cluster, _ []byte, namespace string) ([]SecretSummary, error) {
	var out []SecretSummary
	for _, s := range f.buildSecrets(c) {
		if namespace != "" && s.namespace != namespace {
			continue
		}
		out = append(out, f.secretSummary(s))
	}
	return out, nil
}

func (f *Fake) Secret(_ context.Context, c *domain.Cluster, _ []byte, ref SecretRef) (*SecretDetail, error) {
	for _, s := range f.buildSecrets(c) {
		if s.namespace == ref.Namespace && s.name == ref.Name {
			d := SecretDetail{
				SecretSummary: f.secretSummary(s),
				Labels:        map[string]string{"app.kubernetes.io/managed-by": "kaas-demo"},
				Keys:          s.keys,
			}
			return &d, nil
		}
	}
	return nil, fmt.Errorf("secret %s/%s not found", ref.Namespace, ref.Name)
}

func (f *Fake) SecretManifest(_ context.Context, c *domain.Cluster, _ []byte, ref SecretRef) (string, error) {
	for _, s := range f.buildSecrets(c) {
		if s.namespace == ref.Namespace && s.name == ref.Name {
			// Values are redacted, so the YAML shows the placeholder for every key - the same shape the
			// real backend produces.
			data := map[string]any{}
			for _, k := range s.keys {
				data[k.Key] = RedactedValue
			}
			obj := map[string]any{
				"apiVersion": "v1",
				"kind":       "Secret",
				"type":       s.typ,
				"metadata":   map[string]any{"name": s.name, "namespace": s.namespace},
				"data":       data,
			}
			y, _ := yaml.Marshal(obj)
			return string(y), nil
		}
	}
	return "", fmt.Errorf("secret %s/%s not found", ref.Namespace, ref.Name)
}

// ---- synthesized config model ----------------------------------------------------

func (f *Fake) configMapSummary(cm fakeConfigMap) ConfigMapSummary {
	keys := make([]string, 0, len(cm.data))
	for k := range cm.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return ConfigMapSummary{
		Namespace: cm.namespace,
		Name:      cm.name,
		Keys:      keys,
		DataCount: len(cm.data),
		Immutable: cm.immutable,
		CreatedAt: f.createdAt(cm.namespace, cm.name),
	}
}

func (f *Fake) secretSummary(s fakeSecret) SecretSummary {
	keys := make([]string, 0, len(s.keys))
	for _, k := range s.keys {
		keys = append(keys, k.Key)
	}
	sort.Strings(keys)
	return SecretSummary{
		Namespace: s.namespace,
		Name:      s.name,
		Type:      s.typ,
		Keys:      keys,
		DataCount: len(s.keys),
		ManagedBy: s.managedBy,
		CreatedAt: f.createdAt(s.namespace, s.name),
	}
}

// createdAt is a stable, cluster-relative timestamp so polling doesn't make ages flicker.
func (f *Fake) createdAt(ns, name string) time.Time {
	h := 0
	for _, r := range ns + name {
		h = h*31 + int(r)
	}
	return time.Now().Add(-time.Duration(h%72+1) * time.Hour)
}

// buildConfigMaps synthesizes a plausible ConfigMap set: a couple of application ConfigMaps in the
// user namespaces plus the ones every cluster carries.
func (f *Fake) buildConfigMaps(c *domain.Cluster) []fakeConfigMap {
	out := []fakeConfigMap{
		{namespace: "default", name: "kube-root-ca.crt", data: map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\n… (fake) …\n-----END CERTIFICATE-----"}},
		{namespace: "default", name: "app-config", data: map[string]string{
			"app.properties": "log.level=info\nfeature.newUI=true\ncache.ttl=300",
			"greeting":       "hello from " + c.Name,
		}},
		{namespace: "demo", name: "feature-flags", data: map[string]string{"flags.json": `{"beta":true,"canary":false}`}, immutable: true},
		{namespace: "kube-system", name: "coredns", data: map[string]string{"Corefile": ".:53 {\n  kubernetes cluster.local\n  forward . /etc/resolv.conf\n}"}},
	}
	return out
}

// buildSecrets synthesizes a plausible Secret set. The Vault-synced ones carry a ManagedBy so the page
// can badge them - and no values, ever.
func (f *Fake) buildSecrets(c *domain.Cluster) []fakeSecret {
	return []fakeSecret{
		{namespace: "default", name: "app-credentials", typ: "Opaque", managedBy: "app-credentials", keys: []SecretKeyInfo{
			{Key: "username", Bytes: 8}, {Key: "password", Bytes: 24},
		}},
		{namespace: "default", name: "tls-cert", typ: "kubernetes.io/tls", keys: []SecretKeyInfo{
			{Key: "tls.crt", Bytes: 1224}, {Key: "tls.key", Bytes: 1704},
		}},
		{namespace: "demo", name: "db-credentials", typ: "Opaque", managedBy: "db-credentials", keys: []SecretKeyInfo{
			{Key: "DATABASE_URL", Bytes: 61}, {Key: "DB_PASSWORD", Bytes: 32},
		}},
		{namespace: "kube-system", name: "kaas-vault-token", typ: "Opaque", keys: []SecretKeyInfo{
			{Key: "token", Bytes: 95},
		}},
	}
}
