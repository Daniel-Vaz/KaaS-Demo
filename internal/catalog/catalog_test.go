package catalog

import (
	"strings"
	"testing"
)

// Tests over the SHIPPED catalog assert its SHAPE, never its pins. Bumping a version is the one
// thing catalog.json exists for (see the package doc: editing versions is a data change, not a code
// change), so a test that repeats those versions is a second copy to edit and turns every routine
// bump red for no signal. Anything version-specific here derives from the catalog itself; what is
// written out by hand is the set of PRODUCT decisions a version bump must not change - the bundle's
// add-on names, the monitoring stack installing first, one unambiguous head.
//
// The synthetic catalogs below (Parse of a JSON literal) are the opposite: their versions ARE the
// fixture, chosen to exercise one upgrade hop or one validation rule, and they never move.

func TestDefaultLoadsAndValidates(t *testing.T) {
	if _, err := Default(); err != nil {
		t.Fatalf("embedded catalog invalid: %v", err)
	}
}

// shipped resolves the embedded catalog's head bundle - the one every new cluster is created from.
func shipped(t *testing.T) (*Catalog, Bundle, ResolvedBundle) {
	t.Helper()
	c, err := Default()
	if err != nil {
		t.Fatalf("embedded catalog invalid: %v", err)
	}
	b, ok := c.LatestSupportedBundle()
	if !ok {
		t.Fatal("no supported bundle found")
	}
	rb, err := c.Resolve(b.Name)
	if err != nil {
		t.Fatalf("resolve %s: %v", b.Name, err)
	}
	return c, b, rb
}

// TestLatestSupportedBundle: the shipped catalog has exactly ONE supported head. LatestSupportedBundle
// returns the first supported bundle nothing supersedes, so a second head would make the version every
// new cluster gets depend on the order entries happen to sit in the JSON file.
func TestLatestSupportedBundle(t *testing.T) {
	c, head, _ := shipped(t)
	superseded := map[string]bool{}
	for _, b := range c.Bundles {
		if b.Supersedes != "" {
			superseded[b.Supersedes] = true
		}
	}
	var heads []string
	for _, b := range c.Bundles {
		if b.Status == StatusSupported && !superseded[b.Name] {
			heads = append(heads, b.Name)
		}
	}
	if len(heads) != 1 {
		t.Fatalf("supported heads = %v, want exactly one (LatestSupportedBundle picks by slice order)", heads)
	}
	if head.Name != heads[0] {
		t.Fatalf("latest supported = %q, want %q", head.Name, heads[0])
	}
}

// TestResolveBundle checks Resolve expands EVERY shipped bundle into exactly what that bundle
// declares - the bundle is the expectation, so this holds across any version bump or new release.
func TestResolveBundle(t *testing.T) {
	c, _, _ := shipped(t)
	for _, b := range c.Bundles {
		t.Run(b.Name, func(t *testing.T) {
			rb, err := c.Resolve(b.Name)
			if err != nil {
				t.Fatal(err)
			}
			if rb.Name != b.Name {
				t.Fatalf("name = %q, want %q", rb.Name, b.Name)
			}
			if rb.OS.Name != b.OS {
				t.Fatalf("os = %q, want %q", rb.OS.Name, b.OS)
			}
			if rb.Kubernetes != b.Kubernetes {
				t.Fatalf("k8s = %q, want %q", rb.Kubernetes, b.Kubernetes)
			}
			// The CNI is lifted out of the add-on list and carries the BUNDLE's pin rather than the
			// catalog entry's own version - the whole point of a bundle.
			if rb.CNI.Name != b.CNI || rb.CNI.Type != "cni" || rb.CNI.Version != b.Addons[b.CNI] {
				t.Fatalf("cni = %+v, want %s/%s/cni", rb.CNI, b.CNI, b.Addons[b.CNI])
			}
			if len(rb.Addons) != len(b.Addons)-1 {
				t.Fatalf("addons = %d, want %d (the bundle's set minus the CNI)", len(rb.Addons), len(b.Addons)-1)
			}
			seen := map[string]bool{}
			for _, a := range rb.Addons {
				if a.Type == "cni" {
					t.Fatalf("CNI %q leaked into Addons list", a.Name)
				}
				pinned, ok := b.Addons[a.Name]
				if !ok {
					t.Fatalf("add-on %q resolved but not pinned by the bundle", a.Name)
				}
				if a.Version != pinned {
					t.Fatalf("addon %s version = %q, want the bundle's pin %q", a.Name, a.Version, pinned)
				}
				if seen[a.Name] {
					t.Fatalf("add-on %q resolved twice", a.Name)
				}
				seen[a.Name] = true
				// Resolve expands a name into its catalog entry; an unexpanded one would install
				// from an empty chart ref. An OCI chart (oci://…) carries its registry in the ref
				// and takes no repo - see helm.helmArgs.
				if a.Chart == "" || (a.Repo == "" && !strings.HasPrefix(a.Chart, "oci://")) {
					t.Fatalf("add-on %q not expanded from the catalog: %+v", a.Name, a)
				}
			}
		})
	}
}

// TestDefaultBundleShipsBatteriesIncluded pins the head bundle's add-on SET - names, never versions:
// the batteries-included default every cluster is locked to at create time. metallb + envoy-gateway so
// every cluster comes up with a LoadBalancer implementation and a default Gateway API, external-dns so
// the names its users' apps ask for are published under the cluster's domain, and cert-manager so
// routes exposed on the default Gateway are HTTPS-ready via a self-signed issuer. longhorn so a plain
// PersistentVolumeClaim gets a real replicated volume off the storage disk every worker is born with.
// external-secrets so every cluster is wired to the platform's central Vault (a ClusterSecretStore
// over a per-cluster JWT auth role). Changing this set is a product decision - it is what every
// cluster pays for in host capacity - so it should fail here and be changed deliberately.
func TestDefaultBundleShipsBatteriesIncluded(t *testing.T) {
	_, head, rb := shipped(t)
	if head.CNI != "cilium" {
		t.Errorf("cni = %q, want cilium", head.CNI)
	}
	want := map[string]bool{
		"kube-prometheus-stack": true, "metrics-server": true, "trivy-operator": true,
		"metallb": true, "envoy-gateway": true, "external-dns": true, "cert-manager": true,
		"longhorn": true, "external-secrets": true,
	}
	got := map[string]bool{}
	for _, a := range rb.Addons {
		got[a.Name] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("default bundle no longer ships %q", n)
		}
	}
	for n := range got {
		if !want[n] {
			t.Errorf("default bundle gained %q - every cluster now pays for it; update this test if that is deliberate", n)
		}
	}
}

// TestMonitoringStackInstallsFirst: install order is (Priority, Name), and kube-prometheus-stack
// (priority -100) must come first so its Prometheus Operator + ServiceMonitor CRD exist before
// metrics-server publishes a ServiceMonitor.
func TestMonitoringStackInstallsFirst(t *testing.T) {
	_, _, rb := shipped(t)
	first := rb.Addons[0]
	if first.Name != "kube-prometheus-stack" {
		t.Fatalf("first add-on = %q, want kube-prometheus-stack (installs first)", first.Name)
	}
	// The monitoring stack pins a dedicated namespace and a longer helm timeout.
	if first.Namespace != "monitoring-system" {
		t.Errorf("kube-prometheus-stack namespace = %q, want monitoring-system", first.Namespace)
	}
	if first.Timeout == "" {
		t.Error("kube-prometheus-stack has no timeout override; the manager default is short for it")
	}
	// It declares the ServiceMonitor CRD so the helm manager waits for it to Establish before the
	// next add-on installs (else a ServiceMonitor-publishing chart fails with "no matches for kind").
	found := false
	for _, crd := range first.EstablishCRDs {
		if crd == "servicemonitors.monitoring.coreos.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("kube-prometheus-stack establishCRDs = %v, want it to include the ServiceMonitor CRD", first.EstablishCRDs)
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

// TestDefaultBundleIsHead: the shipped catalog's newest bundle is the head of its chain, so a
// cluster already on it is offered no upgrade. Multi-hop chains are covered by the synthetic
// catalogs below - the shipped one retires bundles as they age.
func TestDefaultBundleIsHead(t *testing.T) {
	c, head, _ := shipped(t)
	if ups := c.UpgradesFor(head.Name); len(ups) != 0 {
		t.Fatalf("upgrades for head %s = %v, want none", head.Name, names(ups))
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
