package kube

import (
	"context"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Storage types for the portal's Storage page - the PersistentVolumeClaim and StorageClass views.
// They live in the kube seam rather than a seam of their own (unlike security/monitoring) because
// PVCs and StorageClasses are core Kubernetes objects: they need no add-on, and reading them is the
// same `kubectl get -o json` against the same cluster the Workloads page already talks to. So the
// same Client interface, the same worker exec agent, and the same view-scoped auth cover them, and
// the Fake synthesizes them from control-plane state like everything else.
//
// Both views are strictly read-only: the page never writes, so unlike Scale there is no write-scoped
// method here and view access is enough throughout.

// PVCPhase is a PersistentVolumeClaim's binding phase, as the API server reports it.
const (
	PVCPhasePending = "Pending"
	PVCPhaseBound   = "Bound"
	PVCPhaseLost    = "Lost"
)

// PVCRef identifies a single PersistentVolumeClaim within a cluster. PVCs are namespaced.
type PVCRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// PVCSummary is one row on the Storage page's Claims tab. Capacity is the *actual* bound capacity
// (status.capacity.storage) where a PV is bound and the requested amount otherwise - a PVC can be
// bound to a PV larger than it asked for, and the real number is what an operator needs to see.
// Both are kept so the detail view can show the request alongside what was granted.
type PVCSummary struct {
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`                // Pending | Bound | Lost
	Volume       string    `json:"volume,omitempty"`      // bound PV name ("" while Pending)
	Capacity     string    `json:"capacity,omitempty"`    // actual bound capacity, e.g. "8Gi"
	Requested    string    `json:"requested,omitempty"`   // spec.resources.requests.storage
	AccessModes  []string  `json:"access_modes"`          // RWO | ROX | RWX | RWOP (short forms)
	StorageClass string    `json:"storage_class"`         // "" = no class (the UI shows "-")
	VolumeMode   string    `json:"volume_mode,omitempty"` // Filesystem | Block
	CreatedAt    time.Time `json:"created_at"`
}

// PVCDetail is a claim's full view: the summary plus its labels/annotations, the pods mounting it,
// and the bound PersistentVolume's own properties.
//
// The PV object is PersistentVolume, NOT "Volume": the embedded PVCSummary.Volume already owns the
// `volume` JSON key (the PV's name), and a second `volume` field here would silently shadow it -
// Go's embedding rules would drop the name from the wire with no error.
type PVCDetail struct {
	PVCSummary
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Conditions  []Condition       `json:"conditions"`
	// UsedBy lists the pods that currently mount this claim - the first thing you want when a claim
	// won't delete or is unexpectedly in use. Best-effort: a pod-list failure leaves it empty.
	UsedBy []string `json:"used_by"`
	// PersistentVolume is the bound PV, nil while the claim is unbound.
	PersistentVolume *PersistentVolume `json:"persistent_volume,omitempty"`
}

// PersistentVolume is the bound PV's properties, shown on the claim's Overview. Source is a short
// human rendering of the volume's backing driver ("csi: rook-ceph.rbd.csi.ceph.com", "hostPath",
// …), since the full source union is large and only its shape is interesting here.
type PersistentVolume struct {
	Name          string    `json:"name"`
	Capacity      string    `json:"capacity,omitempty"`
	Status        string    `json:"status,omitempty"`         // Available | Bound | Released | Failed
	ReclaimPolicy string    `json:"reclaim_policy,omitempty"` // Retain | Delete | Recycle
	StorageClass  string    `json:"storage_class,omitempty"`
	Source        string    `json:"source,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// StorageClass is one row on the Storage page's Classes tab and the whole of its detail view - a
// StorageClass is small enough that the list carries every field the detail shows, so there is no
// separate get (only the YAML is fetched on demand). IsDefault reflects the
// storageclass.kubernetes.io/is-default-class annotation.
type StorageClass struct {
	Name              string            `json:"name"`
	Provisioner       string            `json:"provisioner"`
	ReclaimPolicy     string            `json:"reclaim_policy,omitempty"`      // Delete | Retain
	VolumeBindingMode string            `json:"volume_binding_mode,omitempty"` // Immediate | WaitForFirstConsumer
	AllowExpansion    bool              `json:"allow_expansion"`
	IsDefault         bool              `json:"is_default"`
	Parameters        map[string]string `json:"parameters,omitempty"`
	MountOptions      []string          `json:"mount_options,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
}

// StorageReader reads a Ready cluster's storage objects. It is part of Client (see the interface in
// kube.go); split into its own interface purely to keep the two page-shaped surfaces readable.
// namespace == "" means "all namespaces" for PVCs. Every method is read-only, so view access
// suffices and a read-role member's viewer kubeconfig can serve them all.
type StorageReader interface {
	// PVCs lists the cluster's PersistentVolumeClaims as summary rows.
	PVCs(ctx context.Context, c *domain.Cluster, kubeconfig []byte, namespace string) ([]PVCSummary, error)
	// PVC returns one claim's detail, including its bound PV and the pods mounting it.
	PVC(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref PVCRef) (*PVCDetail, error)
	// PVCManifest returns a claim's YAML.
	PVCManifest(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref PVCRef) (string, error)
	// PVCEvents returns the events for a claim (provisioning failures land here).
	PVCEvents(ctx context.Context, c *domain.Cluster, kubeconfig []byte, ref PVCRef) ([]Event, error)
	// StorageClasses lists every StorageClass with the fields the detail view needs.
	StorageClasses(ctx context.Context, c *domain.Cluster, kubeconfig []byte) ([]StorageClass, error)
	// StorageClassManifest returns a class's YAML. StorageClasses are cluster-scoped, so name alone
	// identifies one.
	StorageClassManifest(ctx context.Context, c *domain.Cluster, kubeconfig []byte, name string) (string, error)
}
