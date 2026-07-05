package kubectl

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"context"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
)

// The kube.StorageReader half of the real Client: PersistentVolumeClaims and StorageClasses read
// with the same Execer, arg-building and JSON shapes as the workload half (see kubectl.go).

// defaultClassAnnotation marks the cluster's default StorageClass - a claim with no
// spec.storageClassName gets this class. The beta form is still emitted by some installers, so both
// are honoured.
const (
	defaultClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	defaultClassAnnotationBeta = "storageclass.beta.kubernetes.io/is-default-class"
)

func (c *Client) PVCs(ctx context.Context, cl *domain.Cluster, kc []byte, namespace string) ([]kube.PVCSummary, error) {
	args := []string{"get", "persistentvolumeclaims"}
	if namespace == "" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "-n", namespace)
	}
	args = append(args, "-o", "json")
	out, err := c.run(ctx, kc, cl.ID, args...)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawPVC `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode persistentvolumeclaims: %w", err)
	}
	res := make([]kube.PVCSummary, 0, len(list.Items))
	for _, it := range list.Items {
		res = append(res, it.summary())
	}
	return res, nil
}

func (c *Client) PVC(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.PVCRef) (*kube.PVCDetail, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "persistentvolumeclaims", ref.Name, "-n", ref.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var obj rawPVC
	if err := json.Unmarshal(out, &obj); err != nil {
		return nil, fmt.Errorf("decode persistentvolumeclaim: %w", err)
	}
	detail := obj.detail()
	// The bound PV and the mounting pods are both best-effort enrichment: an unbound claim has no PV
	// to read, and either lookup can fail (RBAC, a racing delete) without making the claim itself
	// unviewable - which is exactly when an operator most wants to look at it.
	if obj.Spec.VolumeName != "" {
		if pv, perr := c.persistentVolume(ctx, cl, kc, obj.Spec.VolumeName); perr == nil {
			detail.PersistentVolume = pv
		}
	}
	if pods, perr := c.podsUsingPVC(ctx, cl, kc, ref); perr == nil {
		detail.UsedBy = pods
	}
	return &detail, nil
}

// persistentVolume reads the PV a claim is bound to. PVs are cluster-scoped.
func (c *Client) persistentVolume(ctx context.Context, cl *domain.Cluster, kc []byte, name string) (*kube.PersistentVolume, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "persistentvolumes", name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var obj rawPV
	if err := json.Unmarshal(out, &obj); err != nil {
		return nil, fmt.Errorf("decode persistentvolume: %w", err)
	}
	pv := obj.volume()
	return &pv, nil
}

// podsUsingPVC lists the pods in the claim's namespace that mount it. There is no field selector for
// a volume source, so this filters the namespace's pods client-side.
func (c *Client) podsUsingPVC(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.PVCRef) ([]string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "pods", "-n", ref.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawPodVolumes `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode pods: %w", err)
	}
	var pods []string
	for _, p := range list.Items {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == ref.Name {
				pods = append(pods, p.Metadata.Name)
				break
			}
		}
	}
	sort.Strings(pods)
	return pods, nil
}

func (c *Client) PVCManifest(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.PVCRef) (string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "persistentvolumeclaims", ref.Name, "-n", ref.Namespace, "-o", "yaml")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *Client) PVCEvents(ctx context.Context, cl *domain.Cluster, kc []byte, ref kube.PVCRef) ([]kube.Event, error) {
	// A claim's events are addressed to the claim itself (ProvisioningFailed, WaitForFirstConsumer,
	// …), so unlike a workload's this is an exact-name match with no owned-object prefix to cover -
	// a field selector does the filtering server-side.
	out, err := c.run(ctx, kc, cl.ID, "get", "events", "-n", ref.Namespace,
		"--field-selector", "involvedObject.kind=PersistentVolumeClaim,involvedObject.name="+ref.Name,
		"-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawEvent `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode events: %w", err)
	}
	res := make([]kube.Event, 0, len(list.Items))
	for _, e := range list.Items {
		res = append(res, e.event())
	}
	sort.Slice(res, func(i, j int) bool { return res[i].LastSeen.After(res[j].LastSeen) })
	if len(res) > 100 {
		res = res[:100]
	}
	return res, nil
}

func (c *Client) StorageClasses(ctx context.Context, cl *domain.Cluster, kc []byte) ([]kube.StorageClass, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "storageclasses", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []rawStorageClass `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode storageclasses: %w", err)
	}
	res := make([]kube.StorageClass, 0, len(list.Items))
	for _, it := range list.Items {
		res = append(res, it.class())
	}
	return res, nil
}

func (c *Client) StorageClassManifest(ctx context.Context, cl *domain.Cluster, kc []byte, name string) (string, error) {
	out, err := c.run(ctx, kc, cl.ID, "get", "storageclasses", name, "-o", "yaml")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---- kubectl -o json shapes ----------------------------------------------------

type rawPVC struct {
	Metadata storageMeta `json:"metadata"`
	Spec     struct {
		VolumeName       string   `json:"volumeName"`
		StorageClassName *string  `json:"storageClassName"`
		VolumeMode       string   `json:"volumeMode"`
		AccessModes      []string `json:"accessModes"`
		Resources        struct {
			Requests map[string]string `json:"requests"`
		} `json:"resources"`
	} `json:"spec"`
	Status struct {
		Phase       string            `json:"phase"`
		Capacity    map[string]string `json:"capacity"`
		AccessModes []string          `json:"accessModes"`
		Conditions  []condObj         `json:"conditions"`
	} `json:"status"`
}

// storageMeta is the metadata subset the storage views need. It carries annotations (the default-class
// marker, and the provisioner's binding annotations) which the workload metaObj has no use for.
type storageMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
}

func (p rawPVC) summary() kube.PVCSummary {
	// A bound claim reports its granted capacity in status; fall back to the request while Pending,
	// so the column is never blank.
	requested := p.Spec.Resources.Requests["storage"]
	capacity := p.Status.Capacity["storage"]
	if capacity == "" {
		capacity = requested
	}
	// status.accessModes is what was actually granted; spec's is what was asked for.
	modes := p.Status.AccessModes
	if len(modes) == 0 {
		modes = p.Spec.AccessModes
	}
	return kube.PVCSummary{
		Namespace:    p.Metadata.Namespace,
		Name:         p.Metadata.Name,
		Status:       p.Status.Phase,
		Volume:       p.Spec.VolumeName,
		Capacity:     capacity,
		Requested:    requested,
		AccessModes:  shortAccessModes(modes),
		StorageClass: derefStr(p.Spec.StorageClassName),
		VolumeMode:   p.Spec.VolumeMode,
		CreatedAt:    p.Metadata.CreationTimestamp,
	}
}

func (p rawPVC) detail() kube.PVCDetail {
	conds := make([]kube.Condition, 0, len(p.Status.Conditions))
	for _, cd := range p.Status.Conditions {
		conds = append(conds, kube.Condition{
			Type: cd.Type, Status: cd.Status, Reason: cd.Reason, Message: cd.Message,
			Updated: latest(cd.LastTransitionTime, cd.LastUpdateTime),
		})
	}
	return kube.PVCDetail{
		PVCSummary:  p.summary(),
		Labels:      p.Metadata.Labels,
		Annotations: p.Metadata.Annotations,
		Conditions:  conds,
		UsedBy:      []string{},
	}
}

type rawPV struct {
	Metadata storageMeta `json:"metadata"`
	Spec     struct {
		Capacity                      map[string]string `json:"capacity"`
		PersistentVolumeReclaimPolicy string            `json:"persistentVolumeReclaimPolicy"`
		StorageClassName              string            `json:"storageClassName"`
		CSI                           *struct {
			Driver string `json:"driver"`
		} `json:"csi"`
		HostPath *struct {
			Path string `json:"path"`
		} `json:"hostPath"`
		NFS *struct {
			Server string `json:"server"`
			Path   string `json:"path"`
		} `json:"nfs"`
		Local *struct {
			Path string `json:"path"`
		} `json:"local"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func (v rawPV) volume() kube.PersistentVolume {
	return kube.PersistentVolume{
		Name:          v.Metadata.Name,
		Capacity:      v.Spec.Capacity["storage"],
		Status:        v.Status.Phase,
		ReclaimPolicy: v.Spec.PersistentVolumeReclaimPolicy,
		StorageClass:  v.Spec.StorageClassName,
		Source:        v.source(),
		CreatedAt:     v.Metadata.CreationTimestamp,
	}
}

// source renders the volume's backing driver as a short human string. The PV source is a large union
// and only the common in-tree kinds plus CSI are worth naming; anything else is left blank rather
// than guessed at.
func (v rawPV) source() string {
	switch {
	case v.Spec.CSI != nil:
		return "csi: " + v.Spec.CSI.Driver
	case v.Spec.HostPath != nil:
		return "hostPath: " + v.Spec.HostPath.Path
	case v.Spec.Local != nil:
		return "local: " + v.Spec.Local.Path
	case v.Spec.NFS != nil:
		return "nfs: " + v.Spec.NFS.Server + ":" + v.Spec.NFS.Path
	default:
		return ""
	}
}

type rawStorageClass struct {
	Metadata             storageMeta       `json:"metadata"`
	Provisioner          string            `json:"provisioner"`
	ReclaimPolicy        string            `json:"reclaimPolicy"`
	VolumeBindingMode    string            `json:"volumeBindingMode"`
	AllowVolumeExpansion *bool             `json:"allowVolumeExpansion"`
	Parameters           map[string]string `json:"parameters"`
	MountOptions         []string          `json:"mountOptions"`
}

func (s rawStorageClass) class() kube.StorageClass {
	return kube.StorageClass{
		Name:              s.Metadata.Name,
		Provisioner:       s.Provisioner,
		ReclaimPolicy:     s.ReclaimPolicy,
		VolumeBindingMode: s.VolumeBindingMode,
		AllowExpansion:    s.AllowVolumeExpansion != nil && *s.AllowVolumeExpansion,
		IsDefault: s.Metadata.Annotations[defaultClassAnnotation] == "true" ||
			s.Metadata.Annotations[defaultClassAnnotationBeta] == "true",
		Parameters:   s.Parameters,
		MountOptions: s.MountOptions,
		Labels:       s.Metadata.Labels,
		CreatedAt:    s.Metadata.CreationTimestamp,
	}
}

// rawPodVolumes is the slice of a pod needed to tell whether it mounts a given claim.
type rawPodVolumes struct {
	Metadata storageMeta `json:"metadata"`
	Spec     struct {
		Volumes []struct {
			PersistentVolumeClaim *struct {
				ClaimName string `json:"claimName"`
			} `json:"persistentVolumeClaim"`
		} `json:"volumes"`
	} `json:"spec"`
}

// ---- small helpers -------------------------------------------------------------

// shortAccessModes renders access modes in kubectl's abbreviated form (ReadWriteOnce → RWO), which is
// what the column has room for and what operators read.
func shortAccessModes(modes []string) []string {
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		switch strings.TrimSpace(m) {
		case "ReadWriteOnce":
			out = append(out, "RWO")
		case "ReadOnlyMany":
			out = append(out, "ROX")
		case "ReadWriteMany":
			out = append(out, "RWX")
		case "ReadWriteOncePod":
			out = append(out, "RWOP")
		default:
			out = append(out, m)
		}
	}
	return out
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
