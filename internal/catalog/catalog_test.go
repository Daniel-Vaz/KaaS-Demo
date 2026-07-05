package catalog

import "testing"

func TestDefaultLoadsAndValidates(t *testing.T) {
	if _, err := Default(); err != nil {
		t.Fatalf("embedded catalog invalid: %v", err)
	}
}

func TestLatestSupportedBundle(t *testing.T) {
	c, _ := Default()
	b, ok := c.LatestSupportedBundle()
	if !ok {
		t.Fatal("no supported bundle found")
	}
	if b.Name != "2026.1" {
		t.Fatalf("latest supported = %q, want 2026.1", b.Name)
	}
}

func TestResolveBundle(t *testing.T) {
	c, _ := Default()
	rb, err := c.Resolve("2026.1")
	if err != nil {
		t.Fatal(err)
	}
	if rb.OS.Name != "ubuntu-26.04" {
		t.Fatalf("os = %q, want ubuntu-26.04", rb.OS.Name)
	}
	if rb.Kubernetes != "1.36.2" {
		t.Fatalf("k8s = %q, want 1.36.2", rb.Kubernetes)
	}
	if rb.CNI.Name != "cilium" || rb.CNI.Version != "1.19.5" || rb.CNI.Type != "cni" {
		t.Fatalf("cni = %+v, want cilium/1.19.5/cni", rb.CNI)
	}
	// Non-CNI add-ons only, versions pinned by the bundle. metallb + envoy-gateway ship by default so
	// every cluster comes up with a LoadBalancer implementation and a default Gateway API, external-dns
	// so the names its users' apps ask for are published under the cluster's domain, and cert-manager so
	// routes exposed on the default Gateway are HTTPS-ready via a self-signed issuer. longhorn ships
	// by default too, so a plain PersistentVolumeClaim gets a real replicated volume off the storage
	// disk every worker is born with. external-secrets ships by default too, so every cluster is wired
	// to the platform's central Vault (a ClusterSecretStore over a per-cluster JWT auth role).
	want := map[string]string{
		"metrics-server": "3.13.1", "kube-prometheus-stack": "87.16.1", "trivy-operator": "0.34.0",
		"metallb": "0.16.1", "envoy-gateway": "v1.8.2", "external-dns": "1.21.1", "cert-manager": "1.21.0",
		"longhorn": "1.12.0", "external-secrets": "2.7.0",
	}
	if len(rb.Addons) != len(want) {
		t.Fatalf("addons = %d, want %d", len(rb.Addons), len(want))
	}
	for _, a := range rb.Addons {
		if a.Type == "cni" {
			t.Fatalf("CNI %q leaked into Addons list", a.Name)
		}
		if want[a.Name] != a.Version {
			t.Fatalf("addon %s version = %q, want %q", a.Name, a.Version, want[a.Name])
		}
	}
	// Install order is (Priority, Name): kube-prometheus-stack (priority -100) must come first so its
	// Prometheus Operator + ServiceMonitor CRD exist before metrics-server publishes a ServiceMonitor.
	if rb.Addons[0].Name != "kube-prometheus-stack" {
		t.Fatalf("first add-on = %q, want kube-prometheus-stack (installs first)", rb.Addons[0].Name)
	}
	// The monitoring stack pins a dedicated namespace and a longer helm timeout.
	if rb.Addons[0].Namespace != "monitoring-system" {
		t.Fatalf("kube-prometheus-stack namespace = %q, want monitoring-system", rb.Addons[0].Namespace)
	}
	// It declares the ServiceMonitor CRD so the helm manager waits for it to Establish before the
	// next add-on installs (else a ServiceMonitor-publishing chart fails with "no matches for kind").
	found := false
	for _, crd := range rb.Addons[0].EstablishCRDs {
		if crd == "servicemonitors.monitoring.coreos.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("kube-prometheus-stack establishCRDs = %v, want it to include the ServiceMonitor CRD", rb.Addons[0].EstablishCRDs)
	}
}

// TestResolveAddonOrdering checks that SortAddons/Resolve order by (Priority, Name) regardless of the
// order add-ons appear in the bundle map (Go map iteration is randomized).
func TestResolveAddonOrdering(t *testing.T) {
	c, err := Parse([]byte(`{
      "os":[{"name":"o","family":"x","release":"1","status":"supported"}],
      "kubernetes":[{"version":"1.36.0","status":"supported"}],
      "addons":[
        {"name":"cilium","type":"cni","version":"1","status":"supported"},
        {"name":"zzz-late","type":"addon","version":"1","status":"supported","priority":10},
        {"name":"aaa-mid","type":"addon","version":"1","status":"supported"},
        {"name":"prio-first","type":"addon","version":"1","status":"supported","priority":-100}
      ],
      "bundles":[
        {"name":"b","status":"supported","os":"o","kubernetes":"1.36.0","cni":"cilium","addons":{"cilium":"1","zzz-late":"1","aaa-mid":"1","prio-first":"1"},"supersedes":""}
      ]}`))
	if err != nil {
		t.Fatalf("catalog invalid: %v", err)
	}
	rb, err := c.Resolve("b")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{rb.Addons[0].Name, rb.Addons[1].Name, rb.Addons[2].Name}
	want := []string{"prio-first", "aaa-mid", "zzz-late"} // priority -100, then 0 (by name), then 10
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("addon order = %v, want %v", got, want)
		}
	}
}

// TestDefaultBundleIsHead: the default catalog now ships a single supported bundle, so it is
// the head and offers no upgrades.
func TestDefaultBundleIsHead(t *testing.T) {
	c, _ := Default()
	if ups := c.UpgradesFor("2026.1"); len(ups) != 0 {
		t.Fatalf("upgrades for head = %v, want none", names(ups))
	}
}

// TestUpgradeChain exercises UpgradesFor/NextUpgrade over a synthetic multi-bundle catalog,
// keeping the upgrade-promotion feature covered independently of the shipped catalog.
func TestUpgradeChain(t *testing.T) {
	c, err := Parse([]byte(`{
      "os":[{"name":"o","family":"x","release":"1","status":"supported"}],
      "kubernetes":[{"version":"1.35.0","status":"supported"},{"version":"1.36.0","status":"supported"}],
      "addons":[{"name":"cilium","type":"cni","version":"1","status":"supported"}],
      "bundles":[
        {"name":"2025.4","status":"supported","os":"o","kubernetes":"1.35.0","cni":"cilium","addons":{"cilium":"1"},"supersedes":""},
        {"name":"2026.1","status":"supported","os":"o","kubernetes":"1.36.0","cni":"cilium","addons":{"cilium":"1"},"supersedes":"2025.4"}
      ]}`))
	if err != nil {
		t.Fatalf("synthetic catalog invalid: %v", err)
	}
	ups := c.UpgradesFor("2025.4")
	if len(ups) != 1 || ups[0].Name != "2026.1" {
		t.Fatalf("upgrades for 2025.4 = %v, want [2026.1]", names(ups))
	}
	if next, ok := c.NextUpgrade("2025.4"); !ok || next.Name != "2026.1" {
		t.Fatalf("next upgrade = %v/%v, want 2026.1/true", next.Name, ok)
	}
	if ups := c.UpgradesFor("2026.1"); len(ups) != 0 {
		t.Fatalf("upgrades for head = %v, want none", names(ups))
	}
}

// upgradeChainCatalog is a synthetic multi-hop catalog for exercising the upgrade feature, where
// each hop changes exactly one component: 2025.1 (metrics-server 3.12.2) → 2025.2 (metrics-server
// 3.13.1; add-on only) → 2025.3 (1.35.6→1.36.2; Kubernetes only, same OS so a single-node cluster
// can take it) → 2026.1 (ubuntu-22.04→24.04; OS only). Its head, 2026.1, mirrors the shipped
// catalog's single current bundle exactly - the shipped catalog no longer carries this history
// once a bundle is retired, so this fixture stands in for what it looked like mid-chain.
func upgradeChainCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := Parse([]byte(`{
      "os":[
        {"name":"ubuntu-22.04","family":"ubuntu","release":"22.04","status":"supported"},
        {"name":"ubuntu-24.04","family":"ubuntu","release":"24.04","status":"supported"}
      ],
      "kubernetes":[{"version":"1.35.6","status":"supported"},{"version":"1.36.2","status":"supported"}],
      "addons":[
        {"name":"cilium","type":"cni","version":"1.19.5","status":"supported"},
        {"name":"metrics-server","type":"addon","version":"3.13.1","status":"supported"}
      ],
      "bundles":[
        {"name":"2025.1","status":"supported","os":"ubuntu-22.04","kubernetes":"1.35.6","cni":"cilium","addons":{"cilium":"1.19.5","metrics-server":"3.12.2"},"supersedes":""},
        {"name":"2025.2","status":"supported","os":"ubuntu-22.04","kubernetes":"1.35.6","cni":"cilium","addons":{"cilium":"1.19.5","metrics-server":"3.13.1"},"supersedes":"2025.1"},
        {"name":"2025.3","status":"supported","os":"ubuntu-22.04","kubernetes":"1.36.2","cni":"cilium","addons":{"cilium":"1.19.5","metrics-server":"3.13.1"},"supersedes":"2025.2"},
        {"name":"2026.1","status":"supported","os":"ubuntu-24.04","kubernetes":"1.36.2","cni":"cilium","addons":{"cilium":"1.19.5","metrics-server":"3.13.1"},"supersedes":"2025.3"}
      ]}`))
	if err != nil {
		t.Fatalf("synthetic upgrade-chain catalog invalid: %v", err)
	}
	return c
}

// TestMultiHopUpgradeChain checks UpgradesFor walks a multi-bundle chain in order, each hop
// exercising a single upgrade strategy (see upgradeChainCatalog).
func TestMultiHopUpgradeChain(t *testing.T) {
	c := upgradeChainCatalog(t)
	if got := names(c.UpgradesFor("2025.1")); len(got) != 3 || got[0] != "2025.2" || got[1] != "2025.3" || got[2] != "2026.1" {
		t.Fatalf("upgrades for 2025.1 = %v, want [2025.2 2025.3 2026.1]", got)
	}
	if got := names(c.UpgradesFor("2025.3")); len(got) != 1 || got[0] != "2026.1" {
		t.Fatalf("upgrades for 2025.3 = %v, want [2026.1]", got)
	}
	if got := c.UpgradesFor("2026.1"); len(got) != 0 {
		t.Fatalf("upgrades for head 2026.1 = %v, want none", names(got))
	}
}

func TestGoldenImageName(t *testing.T) {
	if got := GoldenImageName("ubuntu-24.04", "1.36.2"); got != "ubuntu-24.04-k8s-1.36.2.qcow2" {
		t.Fatalf("golden image = %q, want ubuntu-24.04-k8s-1.36.2.qcow2", got)
	}
}

// The golden-image artefact differs by provider: KVM clones a qcow2 volume, vSphere clones a VM
// template (which has no file suffix). Getting this wrong means a node is cloned from an image
// that doesn't exist under that name.
func TestGoldenImageNameFor(t *testing.T) {
	if got := GoldenImageNameFor("kvm", "ubuntu-24.04", "1.36.2"); got != "ubuntu-24.04-k8s-1.36.2.qcow2" {
		t.Errorf("kvm golden image = %q, want ubuntu-24.04-k8s-1.36.2.qcow2", got)
	}
	if got := GoldenImageNameFor("vsphere", "ubuntu-24.04", "1.36.2"); got != "ubuntu-24.04-k8s-1.36.2" {
		t.Errorf("vsphere golden image = %q, want ubuntu-24.04-k8s-1.36.2 (a template name, no suffix)", got)
	}
}

func TestDiffResolved(t *testing.T) {
	c := upgradeChainCatalog(t)
	// 2025.1 → 2025.2: only the metrics-server add-on version changes.
	fromA, _ := c.Resolve("2025.1")
	toA, _ := c.Resolve("2025.2")
	dA := DiffResolved(fromA, toA)
	if dA.OSChanged || dA.K8sChanged || dA.CNIChanged {
		t.Fatalf("2025.1→2025.2 diff = %+v, want add-on change only", dA)
	}
	if len(dA.AddonChanges) != 1 || dA.AddonChanges[0].Name != "metrics-server" ||
		dA.AddonChanges[0].From != "3.12.2" || dA.AddonChanges[0].To != "3.13.1" {
		t.Fatalf("2025.1→2025.2 add-on changes = %+v, want metrics-server 3.12.2→3.13.1", dA.AddonChanges)
	}
	// 2025.2 → 2025.3: only Kubernetes changes (same OS, so a single-node cluster can take it).
	fromK, _ := c.Resolve("2025.2")
	toK, _ := c.Resolve("2025.3")
	dK := DiffResolved(fromK, toK)
	if !dK.K8sChanged || dK.OSChanged || dK.CNIChanged || len(dK.AddonChanges) != 0 {
		t.Fatalf("2025.2→2025.3 diff = %+v, want K8sChanged only", dK)
	}
	// 2025.3 → 2026.1: only the OS changes (the HA-only rolling hop).
	fromO, _ := c.Resolve("2025.3")
	toO, _ := c.Resolve("2026.1")
	dO := DiffResolved(fromO, toO)
	if !dO.OSChanged || dO.K8sChanged || dO.CNIChanged || len(dO.AddonChanges) != 0 {
		t.Fatalf("2025.3→2026.1 diff = %+v, want OSChanged only", dO)
	}
	if !dA.Changed() || !dO.Changed() || !dK.Changed() {
		t.Fatal("every hop should report Changed()")
	}
}

func TestValidateRejectsMinorSkip(t *testing.T) {
	bad := []byte(`{
      "os":[{"name":"o","family":"x","release":"1","status":"supported"}],
      "kubernetes":[{"version":"1.30.0","status":"supported"},{"version":"1.32.0","status":"supported"}],
      "addons":[{"name":"cilium","type":"cni","version":"1","status":"supported"}],
      "bundles":[
        {"name":"a","status":"supported","os":"o","kubernetes":"1.30.0","cni":"cilium","addons":{"cilium":"1"},"supersedes":""},
        {"name":"b","status":"supported","os":"o","kubernetes":"1.32.0","cni":"cilium","addons":{"cilium":"1"},"supersedes":"a"}
      ]}`)
	if _, err := Parse(bad); err == nil {
		t.Fatal("expected error: bundle b skips a Kubernetes minor over a")
	}
}

func TestValidateRejectsUnknownRef(t *testing.T) {
	bad := []byte(`{
      "os":[{"name":"o","family":"x","release":"1","status":"supported"}],
      "kubernetes":[{"version":"1.30.0","status":"supported"}],
      "addons":[{"name":"cilium","type":"cni","version":"1","status":"supported"}],
      "bundles":[
        {"name":"a","status":"supported","os":"nope","kubernetes":"1.30.0","cni":"cilium","addons":{"cilium":"1"},"supersedes":""}
      ]}`)
	if _, err := Parse(bad); err == nil {
		t.Fatal("expected error: bundle a references unknown os")
	}
}

func names(bs []Bundle) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out
}
