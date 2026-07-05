// Package proxmox is the real provision.Provisioner for Proxmox VE: it drives OpenTofu against the
// module in infra/proxmox/, one workspace per cluster, cloning a per-(OS,k8s) VM template on the
// operator's node into per-cluster VMs.
//
// It shares the OpenTofu mechanics with the KVM and vSphere backends (internal/provision/tofurunner)
// and is shaped almost exactly like vSphere - a shared-network, clone-a-template provider - differing
// in a few ways that matter:
//
//   - Golden images are Proxmox VM templates, resolved by NAME to a numeric vm_id in the module
//     (catalog.GoldenImageNameFor("proxmox", …)); nothing is staged or uploaded.
//   - Clusters share the operator's bridge instead of each owning a network. Node addressing is
//     either the site's DHCP (IPs read back from the QEMU guest agent) or platform-allocated static
//     IPs injected through Proxmox's native cloud-init; the module handles both.
//   - Credentials go into the tofu process environment (PROXMOX_VE_*), never into the tfvars file,
//     which persists on disk in the workspace. Both auth methods are supported: an API token
//     (PROXMOX_VE_API_TOKEN) or a username/password (PROXMOX_VE_USERNAME/PASSWORD).
//   - Extra disks carry a platform-CHOSEN identity (the disk serial, from the minted wwn), like kvm
//     and unlike vSphere - so the module reports it back deterministically without a read.
//
// Every method is idempotent (`apply`/`destroy`), and Postgres stays the source of truth.
package proxmox

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision/tofurunner"
)

type Config struct {
	Bin       string // "tofu"
	ModuleDir string // abs path to infra/proxmox (copied into each workspace)
	// WorkDir is this backend's own workspace root - kept disjoint from the other provisioners' so
	// none ever sees another's workspaces in ListManaged (the orphan-GC input).
	WorkDir string

	Endpoint string // Proxmox API base, e.g. https://172.23.234.12:8006/
	Insecure bool   // accept a self-signed Proxmox certificate (lab)
	// Auth is EITHER a token OR a username+password. New requires exactly one to be complete.
	APIToken string // "user@realm!tokenid=secret"
	Username string // "user@realm", e.g. kaas@pve
	Password string

	Node      string // the Proxmox node VMs run on, e.g. proxmox01
	Datastore string // datastore for the VMs' disks, e.g. Pool3ParNew
	Bridge    string // the bridge nodes attach to, e.g. vmbr0
	VLAN      int    // VLAN tag for the node NIC on a VLAN-aware bridge (0 = untagged)

	SSHPublicKey string // injected via cloud-init (the kaas user)
	Events       events.Sink
	Log          *slog.Logger // required
}

type Provisioner struct {
	cfg Config
	run *tofurunner.Runner
}

// New validates config and returns a Provisioner. It does not contact Proxmox.
func New(cfg Config) (*Provisioner, error) {
	if cfg.Bin == "" {
		cfg.Bin = "tofu"
	}
	required := map[string]string{
		"ModuleDir": cfg.ModuleDir, "WorkDir": cfg.WorkDir,
		"KAAS_PROXMOX_ENDPOINT": cfg.Endpoint, "KAAS_PROXMOX_NODE": cfg.Node,
		"KAAS_PROXMOX_DATASTORE": cfg.Datastore, "KAAS_PROXMOX_NET_BRIDGE": cfg.Bridge,
		"KAAS_SSH_PUBLIC_KEY": cfg.SSHPublicKey,
	}
	for name, v := range required {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("proxmox: %s is required", name)
		}
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("proxmox: Log is required")
	}
	// Exactly one auth method must be fully specified. Both or neither is a misconfiguration.
	token := strings.TrimSpace(cfg.APIToken) != ""
	userpass := strings.TrimSpace(cfg.Username) != "" && cfg.Password != ""
	switch {
	case token && userpass:
		return nil, fmt.Errorf("proxmox: set EITHER KAAS_PROXMOX_API_TOKEN or KAAS_PROXMOX_USERNAME/PASSWORD, not both")
	case !token && !userpass:
		return nil, fmt.Errorf("proxmox: authentication is required - set KAAS_PROXMOX_API_TOKEN, or KAAS_PROXMOX_USERNAME and KAAS_PROXMOX_PASSWORD")
	}
	// Credentials reach the provider through the environment, so they never land in the workspace's
	// terraform.tfvars.json (which outlives the process, on disk).
	extraEnv := []string{
		"PROXMOX_VE_ENDPOINT=" + cfg.Endpoint,
		fmt.Sprintf("PROXMOX_VE_INSECURE=%t", cfg.Insecure),
	}
	if token {
		extraEnv = append(extraEnv, "PROXMOX_VE_API_TOKEN="+cfg.APIToken)
	} else {
		extraEnv = append(extraEnv, "PROXMOX_VE_USERNAME="+cfg.Username, "PROXMOX_VE_PASSWORD="+cfg.Password)
	}
	return &Provisioner{cfg: cfg, run: &tofurunner.Runner{
		Bin:       cfg.Bin,
		ModuleDir: cfg.ModuleDir,
		WorkDir:   cfg.WorkDir,
		ExtraEnv:  extraEnv,
		Events:    cfg.Events,
		Log:       cfg.Log,
	}}, nil
}

// tfvars mirrors infra/proxmox/variables.tf. No credentials here - see New.
type tfvars struct {
	NodeName    string   `json:"node_name"`
	Datastore   string   `json:"datastore"`
	Bridge      string   `json:"bridge"`
	VLAN        int      `json:"vlan"`
	ClusterID   string   `json:"cluster_id"`
	ClusterName string   `json:"cluster_name"`
	IPMode      string   `json:"ip_mode"` // "dhcp" | "static"
	NetworkCIDR string   `json:"network_cidr"`
	Gateway     string   `json:"gateway"` // static mode
	DNS         []string `json:"dns"`     // static mode

	SSHAuthorizedKey string    `json:"ssh_authorized_key"`
	Nodes            []tfvNode `json:"nodes"`
}

type tfvNode struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	CPUs   int    `json:"cpus"`
	MemMB  int    `json:"mem_mb"`
	DiskGB int    `json:"disk_gb"`
	Image  string `json:"image"` // Proxmox template name, e.g. "ubuntu-26.04-k8s-1.36.2"
	IP     string `json:"ip"`    // static mode: the pre-allocated address; "" in dhcp mode
	// ExtraDisks is ORDER-SIGNIFICANT: the module fixes each disk's SCSI slot from its index, so a
	// reordering would renumber live disks. Always sent sorted by name (see below).
	ExtraDisks []tfvDisk `json:"extra_disks"`
}

type tfvDisk struct {
	Name   string `json:"name"`
	SizeGB int    `json:"size_gb"`
	WWN    string `json:"wwn"` // the module sets it as the disk serial (the by-id identity)
}

func (p *Provisioner) EnsureNodes(ctx context.Context, clusterID string, netSpec provision.NetworkSpec, specs []provision.NodeSpec) ([]provision.ProvisionedNode, error) {
	mode := netSpec.Mode
	if mode == "" {
		mode = "dhcp"
	}
	if netSpec.Name == "" {
		return nil, fmt.Errorf("proxmox: cluster %s has no bridge name", clusterID)
	}
	if mode == "static" {
		// The platform allocates static IPs at admission (internal/app, internal/netpool). A node
		// without one here means the cluster's allocation is missing or stale: fail loudly rather
		// than boot a VM that comes up on the wrong address.
		for _, s := range specs {
			if s.IP == "" {
				return nil, fmt.Errorf("proxmox: node %s has no static IP allocated (ip_mode=static)", s.VMName)
			}
		}
	}
	ws, err := p.run.EnsureWorkspace(clusterID)
	if err != nil {
		return nil, fmt.Errorf("proxmox: %w", err)
	}
	v := tfvars{
		NodeName:         p.cfg.Node,
		Datastore:        p.cfg.Datastore,
		Bridge:           netSpec.Name,
		VLAN:             p.cfg.VLAN,
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
		// Sort by name so a disk keeps its SCSI slot for life: the module derives the slot from the
		// list index, so an unstable order would renumber a live node's disks underneath it.
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
		return nil, fmt.Errorf("proxmox: write vars: %w", err)
	}
	return p.run.Apply(ctx, ws, clusterID)
}

func (p *Provisioner) DestroyCluster(ctx context.Context, clusterID string) error {
	return p.run.DestroyAndRemove(ctx, clusterID)
}

func (p *Provisioner) ListManaged(ctx context.Context) ([]string, error) {
	return p.run.ListManaged(ctx)
}

// ImageAvailable reports whether the named VM template exists on the operator's node, so a rolling
// OS replacement never drains a node it cannot rebuild (see the reconciler's preflight). Unlike the
// KVM backend there is no base-image fallback to check against: on Proxmox a node is a clone of its
// template or it is nothing.
func (p *Provisioner) ImageAvailable(name string) error {
	if name == "" {
		return fmt.Errorf("proxmox: no golden image (VM template) requested")
	}
	ctx := context.Background()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	ok, err := c.templateExists(ctx, p.cfg.Node, name)
	if err != nil {
		return fmt.Errorf("proxmox: check template %q on node %s: %w", name, p.cfg.Node, err)
	}
	if !ok {
		return fmt.Errorf("proxmox: golden image (VM template) %q not found on node %s - build it first "+
			"(make golden-image-proxmox OS_NAME=... K8S_VERSION=...); see docs/infrastructure.md", name, p.cfg.Node)
	}
	return nil
}
