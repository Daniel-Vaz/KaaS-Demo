// Package provision is the infrastructure seam: create/destroy VMs on libvirt/KVM.
//
// The real implementation is OpenTofu + the dmacvicar/libvirt provider (internal/provision/tofu);
// Fake lets the rest of the system run and be tested without touching KVM. Both satisfy this
// interface, and implementations MUST be idempotent.
package provision

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// NetworkSpec is the node network the cluster's VMs attach to.
//
// On kvm it is a dedicated per-cluster network: one isolated libvirt NAT bridge, whose DHCP
// hands out the node addresses. On vsphere it is the operator's existing portgroup, shared by
// every cluster - Name identifies it and Mode says who assigns node IPs ("dhcp": an external
// server, discovered via open-vm-tools; "static": the platform, pre-allocated into NodeSpec.IP).
type NetworkSpec struct {
	CIDR string // e.g. "10.200.3.0/24"
	Mode string // kvm: libvirt forwarding mode ("nat"). vsphere: "dhcp" | "static"

	// vsphere-only.
	Name    string   // portgroup name
	Gateway string   // static mode
	DNS     []string // static mode
	// VIP is the HA control-plane address, and ClusterName the human name. Neither is used to
	// build VMs - they are here for provisioner decorators that record what a cluster occupies
	// on a shared network (see internal/netbox).
	VIP         string
	ClusterName string
	// LoadBalancerIP is the address reserved for the cluster's default MetalLB pool / Envoy Gateway.
	// Like VIP it is recorder-only - not used to build VMs - so the NetBox decorator registers it on
	// the shared subnet. Empty on kvm (a private per-cluster network NetBox never sees).
	LoadBalancerIP string
}

// NodeSpec is the desired shape of a single VM.
type NodeSpec struct {
	VMName string
	Role   domain.Role
	// Pool is the node pool this VM belongs to; empty for a control plane. Provisioners don't act on
	// it (the pool is already baked into VMName, and CPUs/MemMB/DiskGB already carry the pool's
	// sizing) - it rides along so the reconciler can stamp it onto the observed domain.Node, which is
	// what the Ansible inventory reads to label the Kubernetes node (see domain.PoolLabel).
	Pool   string
	CPUs   int
	MemMB  int
	DiskGB int
	Image  string // golden image
	// IP is the address the node MUST come up on, pre-allocated by the platform (vsphere static
	// mode; see internal/netpool.AllocateStatic). Empty means the network's DHCP assigns it.
	IP string
	// Disks are EXTRA block devices this VM should have beyond its root disk (DiskGB), in stable
	// order. Desired state: the provisioner creates what's missing and destroys what's no longer
	// listed, like the node set itself. The platform formats and mounts them separately (Ansible) -
	// the provisioner's job ends at "the device is attached and identifiable".
	Disks []DiskSpec
}

// DiskSpec is one extra block device on a node. Name is unique per node and names the underlying
// volume; WWN is the stable hardware identity the guest will see (kvm only - see domain.NodeDisk).
type DiskSpec struct {
	Name   string
	SizeGB int
	WWN    string
}

// ProvisionedNode is the observed result of provisioning a VM.
type ProvisionedNode struct {
	VMName string
	IP     string
	MAC    string
	// Disks is the observed identity of each extra disk, keyed by DiskSpec.Name. The value is the
	// /dev/disk/by-id/ entry the GUEST exposes ("wwn-0x5000c50…"), which is what Ansible resolves the
	// device through.
	//
	// It is reported rather than assumed because the two providers mint it differently: on kvm the
	// platform chooses the WWN up front, while on vsphere vCenter owns the disk's UUID and the
	// platform can only read it back. Returning it from the provisioner is what keeps everything
	// above this seam provider-agnostic.
	Disks map[string]string
}

// Provisioner creates and destroys the VMs backing a cluster. Implementations MUST
// be idempotent: EnsureNodes creates only what's missing and returns the full set.
type Provisioner interface {
	EnsureNodes(ctx context.Context, clusterID string, net NetworkSpec, specs []NodeSpec) ([]ProvisionedNode, error)
	DestroyCluster(ctx context.Context, clusterID string) error
	// ListManaged returns the cluster IDs this provisioner currently has infrastructure
	// for. The reconciler's orphan GC diffs this against the DB and destroys any infra
	// with no live cluster (self-healing after a crash or an out-of-band change).
	ListManaged(ctx context.Context) ([]string, error)
}

// ImageChecker is an optional Provisioner capability: it reports whether a specific golden
// image is actually available to clone from. The reconciler preflights this before a rolling
// OS replacement, so it never drains/removes a node it cannot rebuild onto the new image.
// (Fake does not implement it - in fake mode there are no real images to check.)
type ImageChecker interface {
	ImageAvailable(name string) error
}

// Unwrapper is implemented by Provisioner DECORATORS (e.g. internal/netbox, which records a
// cluster's IPs) so the reconciler can see through them to an optional capability the inner backend
// implements but the wrapper does not. Without this, a decorator silently strips every optional
// interface it does not itself re-declare - which is how a NetBox-wrapped vSphere provisioner lost
// its NodeReplacer/NodePowerer and left a node drained-but-not-rebuilt. A decorator that forwards
// EnsureNodes/DestroyCluster/ListManaged and implements Unwrap is transparent to every capability.
type Unwrapper interface{ Unwrap() Provisioner }

// AsImageChecker / AsNodeReplacer / AsNodePowerer resolve an optional capability through any chain of
// decorators. Always use these rather than a bare p.(Capability): a bare assertion stops at the
// outermost wrapper and misses a capability the real backend has.
func AsImageChecker(p Provisioner) (ImageChecker, bool) {
	for {
		if c, ok := p.(ImageChecker); ok {
			return c, true
		}
		u, ok := p.(Unwrapper)
		if !ok {
			return nil, false
		}
		p = u.Unwrap()
	}
}

func AsNodeReplacer(p Provisioner) (NodeReplacer, bool) {
	for {
		if c, ok := p.(NodeReplacer); ok {
			return c, true
		}
		u, ok := p.(Unwrapper)
		if !ok {
			return nil, false
		}
		p = u.Unwrap()
	}
}

func AsNodePowerer(p Provisioner) (NodePowerer, bool) {
	for {
		if c, ok := p.(NodePowerer); ok {
			return c, true
		}
		u, ok := p.(Unwrapper)
		if !ok {
			return nil, false
		}
		p = u.Unwrap()
	}
}

// NodePowerer is an optional Provisioner capability: it reports whether each of a cluster's VMs is
// actually running, and can start one that is not.
//
// It is the only signal automatic repair has from BELOW the Kubernetes layer, and that independence
// is what it is for. Every other fault the platform can see is reported by the cluster's own API
// server - so when the API server is the thing that is unreachable, every reading becomes
// simultaneously alarming and untrustworthy, and a repairer with no second opinion can only refuse
// to act. On a single-control-plane cluster that is exactly the case it most needs to fix. The
// hypervisor saying "this VM is off" is a fact from a different source, and it is what turns "I
// cannot see anything" into "I can see the cause".
//
// Optional in the ImageChecker sense: a backend that does not implement it simply never produces the
// FaultVMDown class, and the ladder starts at its next rung. The vSphere and Proxmox backends do not
// implement it (see their repair.go) - reading power state there means a second, differently
// authenticated path to the backend, where everything else goes through OpenTofu.
type NodePowerer interface {
	// NodePower maps VM name to whether the infrastructure reports it as running. A VM the backend
	// has no record of is absent from the map rather than reported as off: "gone" and "stopped" are
	// different faults, and only the second one is repaired by starting it.
	NodePower(ctx context.Context, clusterID string) (map[string]bool, error)
	// PowerOnNode starts one VM. Idempotent: starting a running VM is a no-op.
	PowerOnNode(ctx context.Context, clusterID, vmName string) error
}

// NodeReplacer is an optional Provisioner capability: rebuild ONE node's VM from scratch, keeping
// everything else in the cluster untouched.
//
// It exists because EnsureNodes cannot express this. EnsureNodes is CONVERGE - it creates what is
// missing and returns the full set - so a VM that exists but is broken is, to it, already correct.
// The rolling OS upgrade sidesteps the problem by changing the node's image, which the providers
// treat as ForceNew and replace on their own; a repair rebuilds a node onto the SAME image it is
// already running, so nothing about the desired state has changed and nothing would happen.
//
// Implementations must replace the node's ROOT VOLUME as well as its VM. Replacing the VM alone
// re-attaches the same copy-on-write root disk, which repairs precisely nothing if the fault is a
// corrupt or full root filesystem - the common case. They must equally NOT touch the node's EXTRA
// disks: those hold the node's data (and, on a worker, its Longhorn replicas), and they must survive
// a repair for the same reason they survive a rolling OS replacement. Every backend now models extra
// disks as resources INDEPENDENT of the node's VM - a libvirt_volume, a vsphere_virtual_disk, a
// volume on the per-cluster Proxmox disk-owner VM - so replacing the VM alone preserves them, and no
// backend needs to refuse a disk-bearing node any more.
type NodeReplacer interface {
	// ReplaceNode destroys and re-creates one node's VM and root volume. The caller has already
	// removed the node from the cluster, and rejoins it afterwards. It preserves the node's extra
	// disks, which live outside the VM resource.
	ReplaceNode(ctx context.Context, clusterID, vmName string) error
}

// Fake assigns deterministic pretend IPs from each cluster's own network CIDR (mirroring the real
// per-cluster network), so fake-mode data looks realistic. No real VMs.
type Fake struct {
	mu    sync.Mutex
	nodes map[string]map[string]ProvisionedNode // clusterID -> vmName -> node
}

func NewFake() *Fake {
	return &Fake{nodes: map[string]map[string]ProvisionedNode{}}
}

func (f *Fake) EnsureNodes(_ context.Context, clusterID string, netSpec NetworkSpec, specs []NodeSpec) ([]ProvisionedNode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	byName, ok := f.nodes[clusterID]
	if !ok {
		byName = map[string]ProvisionedNode{}
		f.nodes[clusterID] = byName
	}
	out := make([]ProvisionedNode, 0, len(specs))
	for _, s := range specs {
		n, exists := byName[s.VMName]
		if !exists { // idempotent: only "create" what's missing
			// Hand out .10, .11, … within the cluster's own subnet (leaving the low hosts for the
			// gateway and the top for the HA VIP), so IPs look like a real dedicated network. A
			// pre-allocated address (vsphere static mode) wins, so the platform's allocation
			// round-trips visibly in fake mode.
			offset := 10 + len(byName)
			ip := s.IP
			if ip == "" {
				ip = fakeIP(netSpec.CIDR, offset)
			}
			n = ProvisionedNode{
				VMName: s.VMName,
				IP:     ip,
				MAC:    fmt.Sprintf("52:54:00:00:%02x:%02x", len(byName)/256, offset%256),
			}
			byName[s.VMName] = n
		}
		// Disks are recomputed on EVERY call rather than frozen into the cached node above: a disk is
		// typically added to a node that already exists, so a cached map would report the node's disk
		// set as it was at creation and the fake would never show a newly-added disk.
		n.Disks = fakeDisks(s.Disks)
		out = append(out, n)
	}
	return out, nil
}

// fakeDisks mirrors what the real kvm path produces: the guest's by-id entry for a disk pinned to a
// WWN. Deriving it from the same WWN the platform minted (rather than inventing an id) is what lets
// fake mode exercise the real device-resolution path in the Ansible layer.
func fakeDisks(specs []DiskSpec) map[string]string {
	if len(specs) == 0 {
		return nil
	}
	out := make(map[string]string, len(specs))
	for _, d := range specs {
		out[d.Name] = "wwn-" + d.WWN
	}
	return out
}

// fakeIP returns the offset-th host of cidr, falling back to the legacy libvirt-default subnet
// when no CIDR is set (e.g. bare-struct tests that don't populate a network).
func fakeIP(cidr string, offset int) string {
	if cidr != "" {
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil && ipnet.IP.To4() != nil {
			v := binary.BigEndian.Uint32(ipnet.IP.To4()) + uint32(offset)
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], v)
			return net.IP(b[:]).String()
		}
	}
	return fmt.Sprintf("192.168.122.%d", offset)
}

func (f *Fake) DestroyCluster(_ context.Context, clusterID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nodes, clusterID)
	return nil
}

func (f *Fake) ListManaged(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.nodes))
	for id := range f.nodes {
		out = append(out, id)
	}
	return out, nil
}

// The fake DOES implement NodePowerer and NodeReplacer, unlike ImageChecker. The difference is what
// each capability is for: ImageAvailable answers a question about real files on a real hypervisor,
// which fake mode has nothing to say about, whereas these two are rungs of the automatic-repair
// ladder - and a ladder whose last two rungs are unreachable under `make up-fake` is one nobody can
// see work. So the fake models them: every node it has created is "running", starting one is a
// no-op, and replacing one destroys and re-creates its entry (keeping the SAME pretend IP, which is
// what a real replacement achieves via a pinned MAC).

// NodePower reports every node the fake has created as running. There is nothing here that can stop
// a VM, so the FaultVMDown class only ever appears in fake mode if a test drives it directly.
func (f *Fake) NodePower(_ context.Context, clusterID string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for name := range f.nodes[clusterID] {
		out[name] = true
	}
	return out, nil
}

func (f *Fake) PowerOnNode(_ context.Context, _, _ string) error { return nil }

// ReplaceNode models a VM being destroyed and rebuilt, which in fake mode means KEEPING the node's
// record exactly as it is. That is not laziness - it is the modelled behaviour: a replaced node
// reclaims its address via its pinned MAC, and the stable-IP-on-recreate contract is what every
// rejoin and every sole-control-plane restore depends on. The fake's observable output must
// therefore be unchanged by a replacement.
//
// Dropping the entry and letting EnsureNodes re-create it would be the obvious implementation and
// would be WRONG: the fake allocates addresses by insertion order (10 + len(byName)), so a
// re-created node would take the offset of whichever node was added last and collide with it.
func (f *Fake) ReplaceNode(_ context.Context, clusterID, vmName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.nodes[clusterID][vmName]; !ok {
		return fmt.Errorf("provision: no node %q in cluster %q to replace", vmName, clusterID)
	}
	return nil
}
