package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// The kube.StorageReader half of the Fake: a plausible set of PersistentVolumeClaims and
// StorageClasses synthesized from control-plane state, so the Storage page is demoable with no KVM.
// Deterministic in cluster state, like the workload fake, so the portal's polling doesn't make the
// page flicker.

func (f *Fake) PVCs(_ context.Context, c *domain.Cluster, _ []byte, namespace string) ([]PVCSummary, error) {
	var out []PVCSummary
	for _, p := range f.buildPVCs(c) {
		if namespace != "" && p.namespace != namespace {
			continue
		}
		out = append(out, f.pvcSummary(c, p))
	}
	return out, nil
}

func (f *Fake) PVC(_ context.Context, c *domain.Cluster, _ []byte, ref PVCRef) (*PVCDetail, error) {
	p, ok := f.findPVC(c, ref)
	if !ok {
		return nil, fmt.Errorf("persistentvolumeclaim %s/%s not found", ref.Namespace, ref.Name)
	}
	d := f.pvcDetail(c, p)
	return &d, nil
}

func (f *Fake) PVCManifest(_ context.Context, c *domain.Cluster, _ []byte, ref PVCRef) (string, error) {
	p, ok := f.findPVC(c, ref)
	if !ok {
		return "", fmt.Errorf("persistentvolumeclaim %s/%s not found", ref.Namespace, ref.Name)
	}
	return f.pvcManifest(c, p), nil
}

func (f *Fake) PVCEvents(_ context.Context, c *domain.Cluster, _ []byte, ref PVCRef) ([]Event, error) {
	p, ok := f.findPVC(c, ref)
	if !ok {
		return nil, fmt.Errorf("persistentvolumeclaim %s/%s not found", ref.Namespace, ref.Name)
	}
	return f.pvcEvents(c, p), nil
}

func (f *Fake) StorageClasses(_ context.Context, c *domain.Cluster, _ []byte) ([]StorageClass, error) {
	return f.buildStorageClasses(c), nil
}

func (f *Fake) StorageClassManifest(_ context.Context, c *domain.Cluster, _ []byte, name string) (string, error) {
	for _, sc := range f.buildStorageClasses(c) {
		if sc.Name == name {
			return f.storageClassManifest(sc), nil
		}
	}
	return "", fmt.Errorf("storageclass %s not found", name)
}

// ---- synthesized storage model -------------------------------------------------

type fakePVC struct {
	namespace   string
	name        string
	class       string
	requested   string // what the claim asked for
	capacity    string // what it got ("" = same as requested)
	accessModes []string
	volumeMode  string
	pending     bool     // unbound: no PV, and a WaitForFirstConsumer/ProvisioningFailed event
	usedBy      []string // pods mounting it
}

// buildPVCs returns the synthesized claim set: one per stateful demo workload the workload fake
// builds, plus a pending claim so the page shows a non-Bound row, and add-on claims where the add-on
// really does request storage (kube-prometheus-stack's Prometheus/Grafana, in this demo).
func (f *Fake) buildPVCs(c *domain.Cluster) []fakePVC {
	rwo := []string{"RWO"}
	ps := []fakePVC{
		// The workload fake builds a "cache" StatefulSet with 2 replicas in "demo"; a StatefulSet's
		// volumeClaimTemplate produces one claim per ordinal, so mirror that exactly.
		{
			namespace: "demo", name: "data-cache-0", class: defaultFakeClass, requested: "8Gi",
			accessModes: rwo, volumeMode: "Filesystem", usedBy: []string{"cache-0"},
		},
		{
			namespace: "demo", name: "data-cache-1", class: defaultFakeClass, requested: "8Gi",
			accessModes: rwo, volumeMode: "Filesystem", usedBy: []string{"cache-1"},
		},
		{
			namespace: "demo", name: "uploads", class: defaultFakeClass, requested: "20Gi", capacity: "20Gi",
			accessModes: []string{"RWX"}, volumeMode: "Filesystem", usedBy: []string{"web-" + randSuffix("web", 7) + "-" + randSuffix("web", 0)},
		},
		// A claim stuck Pending - the interesting case a storage page exists to surface.
		{
			namespace: "demo", name: "archive", class: slowFakeClass, requested: "100Gi",
			accessModes: rwo, volumeMode: "Filesystem", pending: true,
		},
	}
	for _, a := range c.Addons {
		if a.Phase == "removing" {
			continue
		}
		switch a.Name {
		case "kube-prometheus-stack":
			ps = append(ps,
				fakePVC{
					namespace: monitoringNS, name: "prometheus-kube-prometheus-stack-prometheus-db-prometheus-kube-prometheus-stack-prometheus-0",
					class: defaultFakeClass, requested: "50Gi", accessModes: rwo, volumeMode: "Filesystem",
					usedBy: []string{"prometheus-kube-prometheus-stack-prometheus-0"},
				},
				fakePVC{
					namespace: monitoringNS, name: "kube-prometheus-stack-grafana",
					class: defaultFakeClass, requested: "10Gi", accessModes: rwo, volumeMode: "Filesystem",
					usedBy: []string{"kube-prometheus-stack-grafana-" + randSuffix("grafana", 3)},
				},
			)
		}
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].namespace != ps[j].namespace {
			return ps[i].namespace < ps[j].namespace
		}
		return ps[i].name < ps[j].name
	})
	return ps
}

func (f *Fake) findPVC(c *domain.Cluster, ref PVCRef) (fakePVC, bool) {
	for _, p := range f.buildPVCs(c) {
		if p.namespace == ref.Namespace && p.name == ref.Name {
			return p, true
		}
	}
	return fakePVC{}, false
}

// volumeName is the PV name a provisioner would have minted for a bound claim: "pvc-<uuid>". Stable
// per claim so the detail view doesn't churn.
func (p fakePVC) volumeName() string {
	if p.pending {
		return ""
	}
	h := fmt.Sprintf("%08x", hash32(p.namespace+"/"+p.name))
	return fmt.Sprintf("pvc-%s-%s-%s-%s-%s%s", h[:8], randSuffix(p.name, 1), randSuffix(p.name, 2),
		randSuffix(p.name, 3), randSuffix(p.name, 4), randSuffix(p.name, 5)[:3])
}

func (p fakePVC) grantedCapacity() string {
	if p.pending {
		return ""
	}
	if p.capacity != "" {
		return p.capacity
	}
	return p.requested
}

func (f *Fake) pvcSummary(c *domain.Cluster, p fakePVC) PVCSummary {
	status := PVCPhaseBound
	if p.pending {
		status = PVCPhasePending
	}
	return PVCSummary{
		Namespace:    p.namespace,
		Name:         p.name,
		Status:       status,
		Volume:       p.volumeName(),
		Capacity:     p.grantedCapacity(),
		Requested:    p.requested,
		AccessModes:  p.accessModes,
		StorageClass: p.class,
		VolumeMode:   p.volumeMode,
		CreatedAt:    c.CreatedAt,
	}
}

func (f *Fake) pvcDetail(c *domain.Cluster, p fakePVC) PVCDetail {
	d := PVCDetail{
		PVCSummary:  f.pvcSummary(c, p),
		Labels:      map[string]string{"app": strings.SplitN(p.name, "-", 2)[0]},
		Annotations: map[string]string{"volume.kubernetes.io/storage-provisioner": fakeProvisioner},
		Conditions:  []Condition{},
		UsedBy:      p.usedBy,
	}
	if d.UsedBy == nil {
		d.UsedBy = []string{}
	}
	if p.pending {
		// A WaitForFirstConsumer claim is unbound because nothing mounts it yet: no volume, and no
		// selected-node annotation either (the scheduler sets that only once a consumer appears).
		return d
	}
	d.PersistentVolume = &PersistentVolume{
		Name:          p.volumeName(),
		Capacity:      p.grantedCapacity(),
		Status:        "Bound",
		ReclaimPolicy: "Delete",
		StorageClass:  p.class,
		Source:        "csi: " + fakeProvisioner,
		CreatedAt:     c.CreatedAt,
	}
	return d
}

func (f *Fake) pvcEvents(c *domain.Cluster, p fakePVC) []Event {
	obj := "PersistentVolumeClaim/" + p.name
	if p.pending {
		return []Event{
			{
				Type: "Normal", Reason: "WaitForFirstConsumer",
				Message: "waiting for first consumer to be created before binding",
				Count:   47, LastSeen: time.Now().Add(-30 * time.Second), Object: obj,
			},
		}
	}
	return []Event{
		{
			Type: "Normal", Reason: "Provisioning",
			Message: fmt.Sprintf("External provisioner is provisioning volume for claim %q", p.namespace+"/"+p.name),
			Count:   1, LastSeen: c.CreatedAt, Object: obj,
		},
		{
			Type: "Normal", Reason: "ProvisioningSucceeded",
			Message: "Successfully provisioned volume " + p.volumeName(),
			Count:   1, LastSeen: c.CreatedAt.Add(3 * time.Second), Object: obj,
		},
	}
}

func (f *Fake) pvcManifest(c *domain.Cluster, p fakePVC) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: PersistentVolumeClaim\n")
	b.WriteString("metadata:\n")
	fmt.Fprintf(&b, "  name: %s\n", p.name)
	fmt.Fprintf(&b, "  namespace: %s\n", p.namespace)
	fmt.Fprintf(&b, "  creationTimestamp: %q\n", c.CreatedAt.UTC().Format(time.RFC3339))
	b.WriteString("  annotations:\n")
	fmt.Fprintf(&b, "    volume.kubernetes.io/storage-provisioner: %s\n", fakeProvisioner)
	b.WriteString("spec:\n  accessModes:\n")
	for _, m := range p.accessModes {
		fmt.Fprintf(&b, "    - %s\n", longAccessMode(m))
	}
	b.WriteString("  resources:\n    requests:\n")
	fmt.Fprintf(&b, "      storage: %s\n", p.requested)
	fmt.Fprintf(&b, "  storageClassName: %s\n", p.class)
	fmt.Fprintf(&b, "  volumeMode: %s\n", p.volumeMode)
	if v := p.volumeName(); v != "" {
		fmt.Fprintf(&b, "  volumeName: %s\n", v)
	}
	b.WriteString("status:\n")
	if p.pending {
		b.WriteString("  phase: Pending\n")
		return b.String()
	}
	b.WriteString("  phase: Bound\n  accessModes:\n")
	for _, m := range p.accessModes {
		fmt.Fprintf(&b, "    - %s\n", longAccessMode(m))
	}
	fmt.Fprintf(&b, "  capacity:\n    storage: %s\n", p.grantedCapacity())
	return b.String()
}

// ---- synthesized storage classes -----------------------------------------------

const (
	// defaultFakeClass is the cluster's default class; slowFakeClass is a second, late-binding class
	// so the page shows more than one row and a non-default one.
	defaultFakeClass = "standard"
	slowFakeClass    = "slow-archive"
	fakeProvisioner  = "csi.hostpath.k8s.io"
	// monitoringNS is where the kube-prometheus-stack add-on's claims live. It must match the
	// catalog's namespace for that add-on (internal/catalog/catalog.json), or the fake would show
	// claims in a namespace the real cluster never has.
	monitoringNS = "monitoring-system"
)

func (f *Fake) buildStorageClasses(c *domain.Cluster) []StorageClass {
	return []StorageClass{
		{
			Name: defaultFakeClass, Provisioner: fakeProvisioner,
			ReclaimPolicy: "Delete", VolumeBindingMode: "Immediate",
			AllowExpansion: true, IsDefault: true,
			Parameters: map[string]string{"type": "thin", "fsType": "ext4"},
			CreatedAt:  c.CreatedAt,
		},
		{
			Name: slowFakeClass, Provisioner: fakeProvisioner,
			ReclaimPolicy: "Retain", VolumeBindingMode: "WaitForFirstConsumer",
			AllowExpansion: false, IsDefault: false,
			Parameters:   map[string]string{"type": "archive", "fsType": "ext4"},
			MountOptions: []string{"noatime"},
			CreatedAt:    c.CreatedAt,
		},
	}
}

func (f *Fake) storageClassManifest(sc StorageClass) string {
	var b strings.Builder
	b.WriteString("apiVersion: storage.k8s.io/v1\nkind: StorageClass\n")
	b.WriteString("metadata:\n")
	fmt.Fprintf(&b, "  name: %s\n", sc.Name)
	fmt.Fprintf(&b, "  creationTimestamp: %q\n", sc.CreatedAt.UTC().Format(time.RFC3339))
	if sc.IsDefault {
		b.WriteString("  annotations:\n    storageclass.kubernetes.io/is-default-class: \"true\"\n")
	}
	fmt.Fprintf(&b, "provisioner: %s\n", sc.Provisioner)
	fmt.Fprintf(&b, "reclaimPolicy: %s\n", sc.ReclaimPolicy)
	fmt.Fprintf(&b, "volumeBindingMode: %s\n", sc.VolumeBindingMode)
	fmt.Fprintf(&b, "allowVolumeExpansion: %t\n", sc.AllowExpansion)
	if len(sc.Parameters) > 0 {
		b.WriteString("parameters:\n")
		for _, k := range sortedKeys(sc.Parameters) {
			fmt.Fprintf(&b, "  %s: %s\n", k, sc.Parameters[k])
		}
	}
	if len(sc.MountOptions) > 0 {
		b.WriteString("mountOptions:\n")
		for _, m := range sc.MountOptions {
			fmt.Fprintf(&b, "  - %s\n", m)
		}
	}
	return b.String()
}

// longAccessMode expands the short access-mode form back to the API's spelling, for the synthesized
// YAML (which should read like a real manifest, not like the table).
func longAccessMode(short string) string {
	switch short {
	case "RWO":
		return "ReadWriteOnce"
	case "ROX":
		return "ReadOnlyMany"
	case "RWX":
		return "ReadWriteMany"
	case "RWOP":
		return "ReadWriteOncePod"
	default:
		return short
	}
}
