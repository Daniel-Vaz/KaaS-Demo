// Package catalog is the authoritative, data-driven inventory of every version the
// platform knows about - OS images, Kubernetes, add-ons, and our own built components -
// plus named release "bundles" that pin a coherent set and chain together via
// `supersedes` to form upgrade paths.
//
// The single source of truth is catalog.json (embedded). Editing versions or cutting a
// new release is a data change, not a code change.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed catalog.json
var raw []byte

// Status is a lifecycle stage shared by every catalog entry.
type Status string

const (
	StatusSupported  Status = "supported"
	StatusDeprecated Status = "deprecated"
	StatusEOL        Status = "eol"
)

func (s Status) valid() bool {
	return s == StatusSupported || s == StatusDeprecated || s == StatusEOL
}

type OSImage struct {
	Name         string `json:"name"`
	Family       string `json:"family"`
	Release      string `json:"release"`
	Status       Status `json:"status"`
	BaseImageURL string `json:"baseImageURL"`
	GoldenImage  string `json:"goldenImage"`
}

type K8sVersion struct {
	Version string `json:"version"`
	Status  Status `json:"status"`
}

type Addon struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "cni" | "addon"
	Version string `json:"version"`
	Status  Status `json:"status"`
	Repo    string `json:"repo"`
	Chart   string `json:"chart"`
	// Description is a short, human-readable summary of what the add-on does, shown in the portal's
	// Catalog page and in the cluster-creation add-on picker. Presentational only.
	Description string            `json:"description,omitempty"`
	Values      map[string]string `json:"values,omitempty"` // helm --set key=value overrides
	// Namespace is the install namespace. Empty means "same as Name" (the historical default the
	// helm manager still applies), so most add-ons live in a namespace of their own name; a chart
	// that must land elsewhere (e.g. kube-prometheus-stack in "monitoring-system") sets this.
	Namespace string `json:"namespace,omitempty"`
	// Priority orders installation: add-ons install low-to-high (default 0). A component others
	// depend on installs first with a negative priority - e.g. kube-prometheus-stack (-100) must
	// bring up the Prometheus Operator + ServiceMonitor CRD before any add-on that publishes a
	// ServiceMonitor. Ties break by Name for determinism.
	Priority int `json:"priority,omitempty"`
	// Timeout overrides the helm --timeout for this add-on (a Go duration like "12m"). Empty uses
	// the manager default; a heavy chart (the monitoring stack) needs longer than the usual 5m.
	Timeout string `json:"timeout,omitempty"`
	// EstablishCRDs names cluster CRDs (fully-qualified `plural.group`) that this add-on installs
	// and that add-ons installed after it depend on. `helm --wait` only waits for workloads, not for
	// CRDs to reach their `Established` condition, and a later add-on's chart references the CRD's
	// kind at render time - so without an explicit wait the next add-on fails with "no matches for
	// kind …". kube-prometheus-stack declares the Prometheus-operator CRDs (ServiceMonitor et al.)
	// here so the helm manager blocks until they are served before the next add-on installs. See
	// internal/addons/helm.awaitCRDsEstablished.
	EstablishCRDs []string `json:"establishCRDs,omitempty"`
}

// Bundle is a named, coherent release: one OS + one Kubernetes + one CNI + pinned add-on
// versions. `Supersedes` names the bundle this one replaces, forming the upgrade chain.
// The CNI's version is pinned in Addons alongside the other add-ons.
type Bundle struct {
	Name       string            `json:"name"`
	Status     Status            `json:"status"`
	OS         string            `json:"os"`
	Kubernetes string            `json:"kubernetes"`
	CNI        string            `json:"cni"`
	Addons     map[string]string `json:"addons"` // add-on name -> pinned version (includes the CNI)
	Supersedes string            `json:"supersedes"`
}

type Catalog struct {
	OS         []OSImage    `json:"os"`
	Kubernetes []K8sVersion `json:"kubernetes"`
	Addons     []Addon      `json:"addons"`
	Bundles    []Bundle     `json:"bundles"`
}

// Default loads and validates the embedded catalog.
func Default() (*Catalog, error) { return Parse(raw) }

// Parse loads and validates a catalog from JSON.
func Parse(b []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("catalog: invalid: %w", err)
	}
	return &c, nil
}

func (c *Catalog) OSImage(name string) (OSImage, bool) {
	for _, o := range c.OS {
		if o.Name == name {
			return o, true
		}
	}
	return OSImage{}, false
}

func (c *Catalog) K8s(version string) (K8sVersion, bool) {
	for _, k := range c.Kubernetes {
		if k.Version == version {
			return k, true
		}
	}
	return K8sVersion{}, false
}

func (c *Catalog) Addon(name string) (Addon, bool) {
	for _, a := range c.Addons {
		if a.Name == name {
			return a, true
		}
	}
	return Addon{}, false
}

func (c *Catalog) Bundle(name string) (Bundle, bool) {
	for _, b := range c.Bundles {
		if b.Name == name {
			return b, true
		}
	}
	return Bundle{}, false
}

// ResolvedBundle is a bundle expanded into concrete catalog entries with pinned versions.
type ResolvedBundle struct {
	Name       string
	OS         OSImage
	Kubernetes string
	CNI        Addon   // Version pinned by the bundle
	Addons     []Addon // non-CNI add-ons, Versions pinned by the bundle
}

// GoldenImageName is the golden-image name for the KVM provider: "<os>-k8s-<kubernetes>.qcow2"
// (e.g. "ubuntu-26.04-k8s-1.36.2.qcow2"). See GoldenImageNameFor.
func GoldenImageName(osName, k8sVersion string) string {
	return GoldenImageNameFor("kvm", osName, k8sVersion)
}

// GoldenImageNameFor is the single source of the golden-image naming convention. A golden image
// is a function of exactly (OS, Kubernetes) - which a bundle pins - so Packer bakes one per pair
// and the provisioner resolves a node's base image from this name. The artefact differs by
// provider: kvm clones a qcow2 volume (so its name carries a file suffix), while vSphere and
// Proxmox clone a VM TEMPLATE resolved by name (no suffix), so the name is provider-aware. Kept a
// plain string (not a type) because it is what lands in domain.Node.Image and drives rolling OS
// replacement.
func GoldenImageNameFor(provider, osName, k8sVersion string) string {
	switch provider {
	case "vsphere", "proxmox":
		return fmt.Sprintf("%s-k8s-%s", osName, k8sVersion)
	default: // kvm
		return fmt.Sprintf("%s-k8s-%s.qcow2", osName, k8sVersion)
	}
}

// AddonChange records a single add-on (or CNI) whose pinned version differs across a hop.
type AddonChange struct {
	Name string
	From string // "" if the add-on is newly introduced by the target bundle
	To   string
}

// BundleDiff is the set of components that change between two resolved bundles. The reconciler
// routes each changed component to its upgrade strategy: OS → rolling node replacement,
// Kubernetes → in-place kubeadm upgrade, CNI/add-ons → helm upgrade.
type BundleDiff struct {
	OSChanged    bool
	K8sChanged   bool
	CNIChanged   bool          // CNI name or its pinned version differs
	AddonChanges []AddonChange // non-CNI add-ons whose pinned version differs (added or bumped)
}

// Changed reports whether the two bundles differ in any component the reconciler acts on.
func (d BundleDiff) Changed() bool {
	return d.OSChanged || d.K8sChanged || d.CNIChanged || len(d.AddonChanges) > 0
}

// DiffResolved computes what changes moving from one resolved bundle to another. It is the input
// to the reconciler's per-component upgrade dispatch.
func DiffResolved(from, to ResolvedBundle) BundleDiff {
	d := BundleDiff{
		OSChanged:  from.OS.Name != to.OS.Name,
		K8sChanged: from.Kubernetes != to.Kubernetes,
		CNIChanged: from.CNI.Name != to.CNI.Name || from.CNI.Version != to.CNI.Version,
	}
	fromAddons := make(map[string]string, len(from.Addons))
	for _, a := range from.Addons {
		fromAddons[a.Name] = a.Version
	}
	for _, a := range to.Addons {
		if prev, ok := fromAddons[a.Name]; !ok || prev != a.Version {
			d.AddonChanges = append(d.AddonChanges, AddonChange{Name: a.Name, From: fromAddons[a.Name], To: a.Version})
		}
	}
	return d
}

// Resolve expands a bundle into concrete, version-pinned components for cluster creation.
func (c *Catalog) Resolve(bundleName string) (ResolvedBundle, error) {
	b, ok := c.Bundle(bundleName)
	if !ok {
		return ResolvedBundle{}, fmt.Errorf("unknown bundle %q", bundleName)
	}
	os, _ := c.OSImage(b.OS)
	cni, _ := c.Addon(b.CNI)
	cni.Version = b.Addons[b.CNI]

	rb := ResolvedBundle{Name: b.Name, OS: os, Kubernetes: b.Kubernetes, CNI: cni}

	for n := range b.Addons {
		if n == b.CNI {
			continue
		}
		a, _ := c.Addon(n)
		a.Version = b.Addons[n]
		rb.Addons = append(rb.Addons, a)
	}
	// Install order is (Priority asc, Name asc): a dependency provider (kube-prometheus-stack:
	// the Prometheus Operator + ServiceMonitor CRD) installs before add-ons that publish a
	// ServiceMonitor. Name breaks ties so the order stays deterministic.
	SortAddons(rb.Addons)
	return rb, nil
}

// SortAddons orders add-ons by (Priority asc, Name asc) in place - the canonical install order.
// Shared by Resolve and the app's per-cluster add-on assembly so both agree.
func SortAddons(addons []Addon) {
	sort.Slice(addons, func(i, j int) bool {
		if addons[i].Priority != addons[j].Priority {
			return addons[i].Priority < addons[j].Priority
		}
		return addons[i].Name < addons[j].Name
	})
}

// LatestSupportedBundle returns the newest supported bundle (a supported "head" that no
// other bundle supersedes).
func (c *Catalog) LatestSupportedBundle() (Bundle, bool) {
	superseded := map[string]bool{}
	for _, b := range c.Bundles {
		if b.Supersedes != "" {
			superseded[b.Supersedes] = true
		}
	}
	for _, b := range c.Bundles {
		if b.Status == StatusSupported && !superseded[b.Name] {
			return b, true
		}
	}
	return Bundle{}, false
}

// UpgradesFor returns the supported bundles reachable by walking the supersedes chain
// forward from `current`, ordered oldest-first (the promotion path). Because the catalog
// forbids a supersedes step from skipping a Kubernetes minor (enforced in validate), each
// hop is a valid single-minor kubeadm upgrade.
func (c *Catalog) UpgradesFor(current string) []Bundle {
	next := map[string]Bundle{}
	for _, b := range c.Bundles {
		if b.Supersedes != "" {
			next[b.Supersedes] = b
		}
	}
	var out []Bundle
	seen := map[string]bool{}
	for cur := current; ; {
		b, ok := next[cur]
		if !ok || seen[b.Name] {
			break
		}
		seen[b.Name] = true
		if b.Status == StatusSupported {
			out = append(out, b)
		}
		cur = b.Name
	}
	return out
}

// NextUpgrade returns the immediate next supported bundle to promote `current` to.
func (c *Catalog) NextUpgrade(current string) (Bundle, bool) {
	ups := c.UpgradesFor(current)
	if len(ups) == 0 {
		return Bundle{}, false
	}
	return ups[0], true
}

func (c *Catalog) validate() error {
	osSet := map[string]bool{}
	for _, o := range c.OS {
		if !o.Status.valid() {
			return fmt.Errorf("os %q: bad status %q", o.Name, o.Status)
		}
		osSet[o.Name] = true
	}
	k8sSet := map[string]bool{}
	for _, k := range c.Kubernetes {
		if !k.Status.valid() {
			return fmt.Errorf("kubernetes %q: bad status %q", k.Version, k.Status)
		}
		k8sSet[k.Version] = true
	}
	addonSet := map[string]Addon{}
	for _, a := range c.Addons {
		if !a.Status.valid() {
			return fmt.Errorf("addon %q: bad status %q", a.Name, a.Status)
		}
		addonSet[a.Name] = a
	}
	bundleSet := map[string]bool{}
	for _, b := range c.Bundles {
		bundleSet[b.Name] = true
	}
	for _, b := range c.Bundles {
		if !b.Status.valid() {
			return fmt.Errorf("bundle %q: bad status %q", b.Name, b.Status)
		}
		if !osSet[b.OS] {
			return fmt.Errorf("bundle %q: unknown os %q", b.Name, b.OS)
		}
		if !k8sSet[b.Kubernetes] {
			return fmt.Errorf("bundle %q: unknown kubernetes %q", b.Name, b.Kubernetes)
		}
		cni, ok := addonSet[b.CNI]
		if !ok || cni.Type != "cni" {
			return fmt.Errorf("bundle %q: cni %q is not a known cni add-on", b.Name, b.CNI)
		}
		if _, ok := b.Addons[b.CNI]; !ok {
			return fmt.Errorf("bundle %q: cni %q must be version-pinned in addons", b.Name, b.CNI)
		}
		for name := range b.Addons {
			if _, ok := addonSet[name]; !ok {
				return fmt.Errorf("bundle %q: unknown add-on %q", b.Name, name)
			}
		}
		if b.Supersedes != "" {
			if !bundleSet[b.Supersedes] {
				return fmt.Errorf("bundle %q: supersedes unknown bundle %q", b.Name, b.Supersedes)
			}
			prev, _ := c.Bundle(b.Supersedes)
			cur, old := minor(b.Kubernetes), minor(prev.Kubernetes)
			if cur < old {
				return fmt.Errorf("bundle %q: kubernetes %s is older than superseded %q (%s)", b.Name, b.Kubernetes, prev.Name, prev.Kubernetes)
			}
			if cur-old > 1 {
				return fmt.Errorf("bundle %q: kubernetes upgrade over %q skips a minor (%s -> %s); kubeadm upgrades one minor at a time", b.Name, prev.Name, prev.Kubernetes, b.Kubernetes)
			}
		}
	}
	return nil
}

// minor returns the minor component of a "major.minor.patch" version (0 if unparseable).
func minor(v string) int {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(parts[1])
	return n
}
