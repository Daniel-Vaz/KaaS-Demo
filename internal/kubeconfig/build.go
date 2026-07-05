package kubeconfig

import (
	"encoding/base64"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ClusterEndpoint extracts the API-server URL and base64-encoded CA data from a kubeconfig's current
// (or, failing that, first) cluster. A freshly-minted per-user kubeconfig must copy these two
// coordinates verbatim so it targets the same endpoint - the HA VIP when there is one - with the
// same trust root as the platform's admin config. It reuses the same minimal shape NewEndpoint
// parses (kubeconfigDoc, transport.go).
func ClusterEndpoint(kc []byte) (server, caData string, err error) {
	var doc kubeconfigDoc
	if err := yaml.Unmarshal(kc, &doc); err != nil {
		return "", "", fmt.Errorf("kubeconfig: parse: %w", err)
	}
	if len(doc.Clusters) == 0 {
		return "", "", fmt.Errorf("kubeconfig: no clusters entry")
	}
	// Resolve the current context's cluster; fall back to the first (the admin config the platform
	// stores has exactly one, so either path lands on the same entry).
	want := ""
	for _, ctx := range doc.Contexts {
		if ctx.Name == doc.CurrentContext {
			want = ctx.Context.Cluster
			break
		}
	}
	sel := doc.Clusters[0]
	for _, cl := range doc.Clusters {
		if cl.Name == want {
			sel = cl
			break
		}
	}
	if sel.Cluster.Server == "" {
		return "", "", fmt.Errorf("kubeconfig: cluster %q has no server", sel.Name)
	}
	return sel.Cluster.Server, sel.Cluster.CertificateAuthorityData, nil
}

// BuildClientCert renders a self-contained kubeconfig for a client-certificate identity: the given
// API server + CA, authenticated by certPEM/keyPEM. This is the shape of a downloaded per-user
// credential - the private key is embedded, so the caller owns a complete, ready-to-use file. The
// user/context names are cosmetic (userName typically the login it authenticates as, for a readable
// `kubectl config` view); the identity the API server actually trusts is the cert's Subject.
func BuildClientCert(clusterName, userName, server, caData string, certPEM, keyPEM []byte) ([]byte, error) {
	if clusterName == "" {
		clusterName = "kubernetes"
	}
	if userName == "" {
		userName = "kaas-user"
	}
	doc := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []any{map[string]any{
			"name": clusterName,
			"cluster": map[string]any{
				"server":                     server,
				"certificate-authority-data": caData,
			},
		}},
		"users": []any{map[string]any{
			"name": userName,
			"user": map[string]any{
				"client-certificate-data": base64.StdEncoding.EncodeToString(certPEM),
				"client-key-data":         base64.StdEncoding.EncodeToString(keyPEM),
			},
		}},
		"contexts": []any{map[string]any{
			"name": userName + "@" + clusterName,
			"context": map[string]any{
				"cluster": clusterName,
				"user":    userName,
			},
		}},
		"current-context": userName + "@" + clusterName,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: render client-cert config: %w", err)
	}
	return out, nil
}
