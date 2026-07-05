// Package vsphere is the real provision.Provisioner for VMware vSphere: it drives OpenTofu
// against the module in infra/vsphere/, one workspace per cluster, cloning a per-(OS,k8s) VM
// template into a per-cluster folder under the deployment's parent folder.
//
// It shares the OpenTofu mechanics with the KVM backend (internal/provision/tofurunner) and
// differs in three ways that matter:
//
//   - Golden images are vCenter VM templates, not qcow2 files - nothing is staged or uploaded,
//     and a node's Image is a template name (catalog.GoldenImageNameFor).
//   - Clusters share the operator's portgroup instead of each owning a network. Node addressing
//     is either the network's own DHCP (IPs discovered via open-vm-tools) or platform-allocated
//     static IPs injected through cloud-init guestinfo; the module handles both.
//   - vCenter credentials go into the tofu process environment (VSPHERE_*), never into the
//     tfvars file, which persists on disk in the workspace.
//
// Every method is idempotent (`apply`/`destroy`), and Postgres stays the source of truth.
package vsphere

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"

	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision/tofurunner"
)

type Config struct {
	Bin       string // "tofu"
	ModuleDir string // abs path to infra/vsphere (copied into each workspace)
	// WorkDir is this backend's own workspace root - kept disjoint from the KVM provisioner's so
	// neither ever sees the other's workspaces in ListManaged (the orphan-GC input).
	WorkDir string

	URL      string // vCenter endpoint, e.g. https://vcenter.example.internal
	Username string // e.g. DOMAIN\user
	Password string
	Insecure bool // accept a self-signed vCenter certificate (lab)

	Datacenter     string // e.g. "MyDC"
	ComputeCluster string // e.g. "CLUSTER01"
	Datastore      string // e.g. "datastorenl"
	// ParentFolder is the VM folder everything lives under: the golden-image templates, and a
	// per-cluster subfolder holding that cluster's VMs.
	ParentFolder string

	SSHPublicKey string // injected via cloud-init guestinfo
	Events       events.Sink
	Log          *slog.Logger // required
}

type Provisioner struct {
	cfg Config
	run *tofurunner.Runner
}

// New validates config and returns a Provisioner. It does not contact vCenter.
func New(cfg Config) (*Provisioner, error) {
	if cfg.Bin == "" {
		cfg.Bin = "tofu"
	}
	required := map[string]string{
		"ModuleDir": cfg.ModuleDir, "WorkDir": cfg.WorkDir,
		"KAAS_VSPHERE_URL": cfg.URL, "KAAS_VSPHERE_USERNAME": cfg.Username,
		"KAAS_VSPHERE_PASSWORD": cfg.Password, "KAAS_VSPHERE_DATACENTER": cfg.Datacenter,
		"KAAS_VSPHERE_CLUSTER": cfg.ComputeCluster, "KAAS_VSPHERE_DATASTORE": cfg.Datastore,
		"KAAS_VSPHERE_FOLDER": cfg.ParentFolder, "KAAS_SSH_PUBLIC_KEY": cfg.SSHPublicKey,
	}
	for name, v := range required {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("vsphere: %s is required", name)
		}
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("vsphere: Log is required")
	}
	return &Provisioner{cfg: cfg, run: &tofurunner.Runner{
		Bin:       cfg.Bin,
		ModuleDir: cfg.ModuleDir,
		WorkDir:   cfg.WorkDir,
		// Credentials reach the provider through the environment, so they never land in the
		// workspace's terraform.tfvars.json (which outlives the process, on disk).
		ExtraEnv: []string{
			"VSPHERE_SERVER=" + hostOf(cfg.URL),
			"VSPHERE_USER=" + cfg.Username,
			"VSPHERE_PASSWORD=" + cfg.Password,
			fmt.Sprintf("VSPHERE_ALLOW_UNVERIFIED_SSL=%t", cfg.Insecure),
		},
		Events: cfg.Events,
		Log:    cfg.Log,
	}}, nil
}

// tfvars mirrors infra/vsphere/variables.tf. No credentials here - see New.
type tfvars struct {
	Datacenter     string `json:"datacenter"`
	ComputeCluster string `json:"compute_cluster"`
	Datastore      string `json:"datastore"`
	Network        string `json:"network"`
	ParentFolder   string `json:"parent_folder"`
	// FolderName is the per-cluster folder created under ParentFolder: "<name>-<id>", so a
	// cluster's VMs are identifiable in vCenter by both.
	FolderName string `json:"folder_name"`
	// ClusterID namespaces VM names and seeds the deterministic MACs (a re-created node keeps
	// its address, which the rolling OS replacement depends on).
	ClusterID        string    `json:"cluster_id"`
	ClusterName      string    `json:"cluster_name"`
	IPMode           string    `json:"ip_mode"` // "dhcp" | "static"
	NetworkCIDR      string    `json:"network_cidr"`
	Gateway          string    `json:"gateway"` // static mode
	DNS              []string  `json:"dns"`     // static mode
	SSHAuthorizedKey string    `json:"ssh_authorized_key"`
	Nodes            []tfvNode `json:"nodes"`
}

type tfvNode struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	CPUs   int    `json:"cpus"`
	MemMB  int    `json:"mem_mb"`
	DiskGB int    `json:"disk_gb"`
	Image  string `json:"image"` // VM template name, e.g. "ubuntu-26.04-k8s-1.36.2"
	IP     string `json:"ip"`    // static mode: the pre-allocated address; "" in dhcp mode
	// ExtraDisks is ORDER-SIGNIFICANT: the module fixes each disk's SCSI unit_number from its index,
	// so a reordering would renumber live disks. Always sent sorted by name (see below).
	ExtraDisks []tfvDisk `json:"extra_disks"`
}

type tfvDisk struct {
	Name   string `json:"name"`
	SizeGB int    `json:"size_gb"`
	WWN    string `json:"wwn"` // unused on vsphere (vCenter mints the identity); carried for one shared type
}

func (p *Provisioner) EnsureNodes(ctx context.Context, clusterID string, netSpec provision.NetworkSpec, specs []provision.NodeSpec) ([]provision.ProvisionedNode, error) {
	mode := netSpec.Mode
	if mode == "" {
		mode = "dhcp"
	}
	if netSpec.Name == "" {
		return nil, fmt.Errorf("vsphere: cluster %s has no network name", clusterID)
	}
	if mode == "static" {
		// The platform allocates static IPs at admission (internal/app, internal/netpool). A node
		// without one here means the cluster's allocation is missing or stale: fail loudly rather
		// than boot a VM that comes up on the wrong address.
		for _, s := range specs {
			if s.IP == "" {
				return nil, fmt.Errorf("vsphere: node %s has no static IP allocated (ip_mode=static)", s.VMName)
			}
		}
	}
	ws, err := p.run.EnsureWorkspace(clusterID)
	if err != nil {
		return nil, fmt.Errorf("vsphere: %w", err)
	}
	v := tfvars{
		Datacenter:       p.cfg.Datacenter,
		ComputeCluster:   p.cfg.ComputeCluster,
		Datastore:        p.cfg.Datastore,
		Network:          netSpec.Name,
		ParentFolder:     p.cfg.ParentFolder,
		FolderName:       folderName(netSpec.ClusterName, clusterID),
		ClusterID:        clusterID,
		ClusterName:      netSpec.ClusterName,
		IPMode:           mode,
		NetworkCIDR:      netSpec.CIDR,
		Gateway:          netSpec.Gateway,
		DNS:              netSpec.DNS,
		SSHAuthorizedKey: strings.TrimSpace(p.cfg.SSHPublicKey),
	}
	if v.DNS == nil {
		v.DNS = []string{}
	}
	for _, s := range specs {
		// Sort by name so a disk keeps its SCSI unit_number for life: the module derives the unit
		// from the list index, so an unstable order would renumber a live node's disks underneath it.
		disks := make([]tfvDisk, 0, len(s.Disks))
		for _, d := range s.Disks {
			disks = append(disks, tfvDisk{Name: d.Name, SizeGB: d.SizeGB, WWN: d.WWN})
		}
		sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
		v.Nodes = append(v.Nodes, tfvNode{
			Name: s.VMName, Role: string(s.Role), CPUs: s.CPUs, MemMB: s.MemMB, DiskGB: s.DiskGB,
			Image: s.Image, IP: s.IP, ExtraDisks: disks,
		})
	}
	if err := p.run.WriteVars(ws, v); err != nil {
		return nil, fmt.Errorf("vsphere: write vars: %w", err)
	}
	return p.run.Apply(ctx, ws, clusterID)
}

func (p *Provisioner) DestroyCluster(ctx context.Context, clusterID string) error {
	return p.run.DestroyAndRemove(ctx, clusterID)
}

func (p *Provisioner) ListManaged(ctx context.Context) ([]string, error) {
	return p.run.ListManaged(ctx)
}

// ImageAvailable reports whether the named VM template exists in the parent folder, so a rolling
// OS replacement never drains a node it cannot rebuild (see the reconciler's preflight). Unlike
// the KVM backend there is no base-image fallback to check against: on vSphere a node is a clone
// of its template or it is nothing.
func (p *Provisioner) ImageAvailable(name string) error {
	if name == "" {
		return fmt.Errorf("vsphere: no golden image (VM template) requested")
	}
	ctx := context.Background()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Logout(ctx) }()

	finder := find.NewFinder(c.Client, true)
	dc, err := finder.Datacenter(ctx, p.cfg.Datacenter)
	if err != nil {
		return fmt.Errorf("vsphere: datacenter %q: %w", p.cfg.Datacenter, err)
	}
	finder.SetDatacenter(dc)
	tmplPath := path.Join(p.cfg.ParentFolder, name)
	if _, err := finder.VirtualMachine(ctx, tmplPath); err != nil {
		return fmt.Errorf("vsphere: golden image (VM template) %q not found under %s in %s - build it first "+
			"(make golden-image-vsphere OS_NAME=... K8S_VERSION=...); see docs/infrastructure.md: %w",
			name, p.cfg.ParentFolder, p.cfg.Datacenter, err)
	}
	return nil
}

// client opens a vCenter session. Used only by ImageAvailable - provisioning itself goes through
// OpenTofu, which holds its own session.
func (p *Provisioner) client(ctx context.Context) (*govmomi.Client, error) {
	u, err := soapURL(p.cfg.URL)
	if err != nil {
		return nil, err
	}
	u.User = url.UserPassword(p.cfg.Username, p.cfg.Password)
	c, err := govmomi.NewClient(ctx, u, p.cfg.Insecure)
	if err != nil {
		return nil, fmt.Errorf("vsphere: connect to %s: %w", p.cfg.URL, err)
	}
	return c, nil
}

// soapURL turns a vCenter base URL into its SOAP endpoint (…/sdk).
func soapURL(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("vsphere: KAAS_VSPHERE_URL %q: %w", raw, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/sdk"
	}
	return u, nil
}

// hostOf is the bare host the hashicorp/vsphere provider wants in VSPHERE_SERVER (no scheme).
func hostOf(raw string) string {
	u, err := soapURL(raw)
	if err != nil {
		return raw
	}
	return u.Host
}

// folderName is the per-cluster vCenter folder: the cluster's name and id, so a folder is
// identifiable in the vSphere UI and unique even across identically-named clusters (two tenants
// may each have a "dev").
func folderName(clusterName, clusterID string) string {
	if clusterName == "" {
		return clusterID
	}
	return clusterName + "-" + clusterID
}
