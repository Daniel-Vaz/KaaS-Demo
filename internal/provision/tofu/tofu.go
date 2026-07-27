// Package tofu is the real provision.Provisioner for KVM: it drives OpenTofu against
// the libvirt module in infra/libvirt/, one workspace per cluster.
//
// The OpenTofu mechanics (workspace, module copy, tfvars, init/apply/destroy, output parsing,
// GC listing) live in internal/provision/tofurunner, shared with the vSphere backend; what stays
// here is libvirt-specific: the module's variables, golden-image resolution under ImageDir, and
// pre-staging images into a remote hypervisor's pool.
//
// Postgres remains the single source of truth; the per-cluster OpenTofu state
// under WorkDir is a derived artifact. If it's lost, the reconciler re-creates the
// workspace and `apply` re-converges. Every method is idempotent (`apply`/`destroy`).
package tofu

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision/tofurunner"
)

// ImageStager makes a golden image available as a volume in the hypervisor's storage pool and
// reports the path it lives at there, so the module can back node volumes onto it directly instead
// of importing it through OpenTofu. Satisfied by *kvmhost.Host; nil for a local hypervisor, where
// the provider imports images itself.
type ImageStager interface {
	StageImage(ctx context.Context, pool, name, localPath string, emit func(string)) (string, error)
}

type Config struct {
	Bin        string // "tofu"
	ModuleDir  string // abs path to infra/libvirt (copied into each workspace)
	WorkDir    string // base dir for per-cluster workspaces
	LibvirtURI string // "qemu:///system"
	Pool       string // libvirt storage pool, e.g. "default"
	BaseImage  string // fallback base/golden qcow2 (used when a node's image isn't in ImageDir)
	ImageDir   string // optional dir of per-(OS,k8s) golden images (see catalog.GoldenImageName)
	// Stager, when set (a remote KVM host), uploads each golden image into the hypervisor's pool ONCE
	// and has the module back node volumes onto it there - instead of the provider streaming it over
	// the libvirt connection once per cluster. See kvmhost.StageImage.
	Stager       ImageStager
	SSHPublicKey string       // injected via cloud-init
	Events       events.Sink  // optional; streams tofu output as events
	Log          *slog.Logger // required
}

type Provisioner struct {
	cfg Config
	run *tofurunner.Runner
}

// New validates config and returns a Provisioner. It does not touch libvirt.
func New(cfg Config) (*Provisioner, error) {
	if cfg.Bin == "" {
		cfg.Bin = "tofu"
	}
	if cfg.LibvirtURI == "" {
		cfg.LibvirtURI = "qemu:///system"
	}
	if cfg.Pool == "" {
		cfg.Pool = "default"
	}
	for name, v := range map[string]string{"ModuleDir": cfg.ModuleDir, "WorkDir": cfg.WorkDir, "BaseImage": cfg.BaseImage, "SSHPublicKey": cfg.SSHPublicKey} {
		if v == "" {
			return nil, fmt.Errorf("tofu: %s is required", name)
		}
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("tofu: Log is required")
	}
	return &Provisioner{cfg: cfg, run: &tofurunner.Runner{
		Bin:       cfg.Bin,
		ModuleDir: cfg.ModuleDir,
		WorkDir:   cfg.WorkDir,
		Events:    cfg.Events,
		Log:       cfg.Log,
	}}, nil
}

// tfvars mirrors infra/libvirt/variables.tf.
type tfvars struct {
	ClusterName string `json:"cluster_name"`
	LibvirtURI  string `json:"libvirt_uri"`
	Pool        string `json:"pool"`
	NetworkCIDR string `json:"network_cidr"`
	NetworkMode string `json:"network_mode"`
	// BaseImage and Nodes[].Image are PATHS (where OpenTofu runs) normally, and VOLUME NAMES already
	// in Pool when PreloadedImages is set - see the module's `preloaded_images`.
	BaseImage        string    `json:"base_image"`
	PreloadedImages  bool      `json:"preloaded_images"`
	SSHAuthorizedKey string    `json:"ssh_authorized_key"`
	Nodes            []tfvNode `json:"nodes"`
}

type tfvNode struct {
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	CPUs       int       `json:"cpus"`
	MemMB      int       `json:"mem_mb"`
	DiskGB     int       `json:"disk_gb"`
	Image      string    `json:"image"` // per-node golden image path; "" falls back to base_image in the module
	ExtraDisks []tfvDisk `json:"extra_disks"`
}

type tfvDisk struct {
	Name   string `json:"name"`
	SizeGB int    `json:"size_gb"`
	WWN    string `json:"wwn"`
}

func (p *Provisioner) EnsureNodes(ctx context.Context, clusterID string, netSpec provision.NetworkSpec, specs []provision.NodeSpec) ([]provision.ProvisionedNode, error) {
	ws, err := p.run.EnsureWorkspace(clusterID)
	if err != nil {
		return nil, fmt.Errorf("tofu: %w", err)
	}
	// With a remote hypervisor the images must already be in its pool before apply - the module backs
	// node volumes onto them by path and imports nothing. Idempotent, and the only slow part of a
	// first run. Locally this is a no-op and the map comes back empty.
	staged, err := p.stageImages(ctx, clusterID, specs)
	if err != nil {
		return nil, err
	}
	if err := p.run.WriteVars(ws, p.vars(clusterID, netSpec, specs, staged)); err != nil {
		return nil, fmt.Errorf("tofu: write vars: %w", err)
	}
	// Extra-disk attachment is converged out of band (the module marks the domain's disk list
	// ignore_changes - see disks.go), and the order around apply is load-bearing: a disk's volume
	// must exist for exactly as long as the disk is attached. So DETACH removed disks first (while
	// their volumes still exist), let apply delete those volumes and create new ones, then ATTACH the
	// new ones. Detaching after apply would leave a domain pointing at a volume apply just deleted,
	// which wedges every later refresh including destroy.
	if err := p.detachRemovedDisks(ctx, clusterID, ws, specs); err != nil {
		return nil, fmt.Errorf("tofu: detach disks: %w", err)
	}
	nodes, err := p.run.Apply(ctx, ws, clusterID)
	if err != nil {
		return nil, err
	}
	if err := p.attachNewDisks(ctx, clusterID, ws, specs); err != nil {
		return nil, fmt.Errorf("tofu: attach disks: %w", err)
	}
	return nodes, nil
}

func (p *Provisioner) DestroyCluster(ctx context.Context, clusterID string) error {
	// Extra disks are attached out of band and their volumes are owned by tofu (see disks.go). Detach
	// every extra disk from every domain first, so tofu's destroy-time refresh never has to resolve a
	// volume a prior removal already deleted - a dangling disk reference makes the refresh fail and
	// wedges destroy permanently. Best-effort: a cleanly-torn-down cluster has nothing to detach.
	p.detachExtraDisksBeforeDestroy(ctx, clusterID)
	return p.run.DestroyAndRemove(ctx, clusterID)
}

// ListManaged returns the cluster IDs this provisioner has infrastructure for (see
// tofurunner.Runner.ListManaged).
func (p *Provisioner) ListManaged(ctx context.Context) ([]string, error) {
	return p.run.ListManaged(ctx)
}

// vars renders the module's inputs. staged maps a LOCAL golden-image path to the path the same image
// occupies in the hypervisor's pool; it is empty for a local hypervisor, where the images the module
// sees are the local paths and the provider imports them itself.
func (p *Provisioner) vars(clusterID string, netSpec provision.NetworkSpec, specs []provision.NodeSpec, staged map[string]string) tfvars {
	mode := netSpec.Mode
	if mode == "" {
		mode = "nat"
	}
	// In preloaded mode every image the module is handed must be a path IN THE POOL, since that is
	// what a node's root volume backs onto. remote() does that translation; locally it is identity.
	remote := func(local string) string {
		if p, ok := staged[local]; ok {
			return p
		}
		return local
	}
	v := tfvars{
		ClusterName:      clusterID,
		LibvirtURI:       p.cfg.LibvirtURI,
		Pool:             p.cfg.Pool,
		NetworkCIDR:      netSpec.CIDR,
		NetworkMode:      mode,
		BaseImage:        remote(p.cfg.BaseImage),
		PreloadedImages:  p.cfg.Stager != nil,
		SSHAuthorizedKey: strings.TrimSpace(p.cfg.SSHPublicKey),
	}
	for _, s := range specs {
		img := p.resolveImage(s.Image)
		// Make image resolution visible in the timeline: whether a node boots its per-version golden
		// image or falls back to the single base image is the difference between an OS upgrade that
		// works and one that silently keeps the old OS. A fallback where a specific image WAS wanted
		// is a warning - the node will not be on the OS the store records.
		switch {
		case s.Image != "" && img == "":
			p.run.Emit(clusterID, "warn", fmt.Sprintf("node %s: golden image %q not found under KAAS_IMAGE_DIR=%q - falling back to base image %s (OS will NOT change)", s.VMName, s.Image, p.cfg.ImageDir, p.cfg.BaseImage))
		case img != "":
			p.run.Emit(clusterID, "info", fmt.Sprintf("node %s: using golden image %s", s.VMName, img))
		}
		disks := make([]tfvDisk, 0, len(s.Disks))
		for _, d := range s.Disks {
			disks = append(disks, tfvDisk{Name: d.Name, SizeGB: d.SizeGB, WWN: d.WWN})
		}
		// An unresolved image stays empty so the module applies its own base_image fallback; a
		// resolved one is translated to the hypervisor's copy when there is one.
		if img != "" {
			img = remote(img)
		}
		v.Nodes = append(v.Nodes, tfvNode{
			Name: s.VMName, Role: string(s.Role), CPUs: s.CPUs, MemMB: s.MemMB, DiskGB: s.DiskGB,
			Image: img, ExtraDisks: disks,
		})
	}
	return v
}

// stageImages uploads every distinct golden image this cluster's nodes need into the hypervisor's
// storage pool, and returns local path → hypervisor path for each. A no-op without a Stager (the
// local hypervisor, where the provider imports images itself over the unix socket), which returns an
// empty map - and an empty map is what makes vars' translation the identity there.
//
// Each image is staged once for the whole platform, not once per cluster: the second cluster to use
// an image finds it already there and provisions with no upload at all. Idempotent, so a reconcile
// retry after a failed/killed upload simply resumes the work by redoing it.
func (p *Provisioner) stageImages(ctx context.Context, clusterID string, specs []provision.NodeSpec) (map[string]string, error) {
	if p.cfg.Stager == nil {
		return nil, nil
	}
	staged := map[string]string{}
	for _, s := range specs {
		path := p.resolveImage(s.Image)
		if path == "" {
			path = p.cfg.BaseImage // same fallback the module applies to an empty node image
		}
		if _, done := staged[path]; done {
			continue
		}
		remote, err := p.cfg.Stager.StageImage(ctx, p.cfg.Pool, filepath.Base(path), path, func(line string) {
			p.run.Emit(clusterID, "info", line)
		})
		if err != nil {
			return nil, fmt.Errorf("tofu: stage golden image: %w", err)
		}
		staged[path] = remote
	}
	return staged, nil
}

// resolveImage maps a node's golden-image name (catalog.GoldenImageName, e.g.
// "ubuntu-26.04-k8s-1.36.2.qcow2") to a concrete path under ImageDir when that file exists.
// Otherwise it returns "" so the module falls back to the single BaseImage - keeping single-image
// setups (no ImageDir, or a version whose image hasn't been baked) working unchanged.
func (p *Provisioner) resolveImage(name string) string {
	if name == "" || p.cfg.ImageDir == "" {
		return ""
	}
	path := filepath.Join(p.cfg.ImageDir, name)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// ImageAvailable reports whether the named golden image can actually be cloned from. Unlike the
// Kubernetes version (which the common role converges over apt), the OS cannot be changed in place,
// so a rolling OS upgrade has no fallback: a missing image must fail loudly rather than silently
// reuse the old base image (which would leave the node on the old OS while the store recorded the
// new one). See resolveImage for the create-time fallback this deliberately does NOT apply here.
func (p *Provisioner) ImageAvailable(name string) error {
	if name == "" {
		return nil // no specific image requested; the module's base_image is fine
	}
	if p.cfg.ImageDir == "" {
		return fmt.Errorf("per-version golden images are not configured (KAAS_IMAGE_DIR unset), so an OS rolling upgrade onto %q is not possible", name)
	}
	path := filepath.Join(p.cfg.ImageDir, name)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("golden image %q not found in %s - build it first (make golden-image OS_NAME=... K8S_VERSION=...); see docs/infrastructure.md", name, p.cfg.ImageDir)
	}
	return nil
}
