// Package kubeconfig adapts a cluster's stored kubeconfig to the network locality of whatever is
// about to use it.
//
// A cluster's kubeconfig points at an address on that cluster's private libvirt subnet
// (https://<cp-ip>:6443, or the VIP for HA). When the KVM host is the local machine, everything
// host-networked can dial that directly. When it is remote, nothing here can - the subnet lives
// behind the hypervisor. WithProxy stamps `proxy-url` onto every cluster entry, pointing at the
// SOCKS5 tunnel internal/kvmhost keeps open to the KVM host; client-go honours that field, so
// kubectl AND helm (and therefore the metrics, health, workloads, monitoring and security seams,
// and the interactive shell) route through it with no other change.
//
// The stored kubeconfig is left canonical - the proxy is applied only to the copies written out
// for platform-side use, never to the one a tenant downloads. Their client sits somewhere else
// entirely, so our tunnel's loopback address would be meaningless to them.
package kubeconfig

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// WithProxy returns kc with `proxy-url: <proxyURL>` set on every cluster entry. An empty proxyURL
// returns kc byte-for-byte unchanged - the local-KVM path, where no rewriting should happen at all.
func WithProxy(kc []byte, proxyURL string) ([]byte, error) {
	if proxyURL == "" || len(kc) == 0 {
		return kc, nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(kc, &doc); err != nil {
		return nil, fmt.Errorf("kubeconfig: parse: %w", err)
	}
	entries, ok := doc["clusters"].([]any)
	if !ok || len(entries) == 0 {
		return nil, fmt.Errorf("kubeconfig: no clusters entry to attach proxy-url to")
	}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		cluster, ok := entry["cluster"].(map[string]any)
		if !ok {
			continue
		}
		cluster["proxy-url"] = proxyURL
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: render: %w", err)
	}
	return out, nil
}
