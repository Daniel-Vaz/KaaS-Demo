package app

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/quota"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// newVSphereApp is an App offering both providers, with the lab's DHCP-mode network shape.
func newVSphereApp(t *testing.T, mode string) (*App, *domain.User) {
	t.Helper()
	a := &App{
		Store:          store.NewMemory(),
		Catalog:        upgradeChainCatalog(t),
		InfraProviders: []string{domain.ProviderKVM, domain.ProviderVSphere},
		ProviderBudgets: map[string]quota.Budget{
			domain.ProviderKVM:     {TotalVCPU: 64, TotalMemMB: 128 * 1024, TotalDiskGB: 1 << 20},
			domain.ProviderVSphere: {TotalVCPU: 64, TotalMemMB: 128 * 1024, TotalDiskGB: 1 << 20},
		},
		sharedNet: map[string]sharedNetSettings{
			domain.ProviderVSphere: {
				Network: "serviceVMNetwork",
				NetMode: mode,
				NetCIDR: "172.23.252.0/24",
				Budget:  quota.Budget{TotalVCPU: 64, TotalMemMB: 128 * 1024},
			},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mode == "static" {
		s := a.sharedNet[domain.ProviderVSphere]
		s.NetGateway = "172.23.252.1"
		s.NetDNS = "172.23.252.10"
		s.NetRange = "172.23.252.200-172.23.252.220"
		a.sharedNet[domain.ProviderVSphere] = s
	}
	// Quota is per infrastructure: this owner is granted capacity on both, so these tests exercise
	// the provider-shaped admission logic rather than tripping over a missing grant.
	owner := &domain.User{ID: "u1", Username: "owner", Quotas: map[string]domain.ResourceQuota{
		domain.ProviderKVM:     {VCPU: 64, MemMB: 128 * 1024, DiskGB: 8192},
		domain.ProviderVSphere: {VCPU: 64, MemMB: 128 * 1024, DiskGB: 8192},
	}}
	if err := a.Store.CreateUser(owner); err != nil {
		t.Fatal(err)
	}
	return a, owner
}

// Proxmox is a shared-network provider like vSphere: admission must stamp the deployment's bridge,
// ip_mode and (static) gateway onto the cluster, and - in static mode - allocate a node IP. This
// proves the generalized admitSharedNetwork path is wired for proxmox, not just vsphere.
func TestProxmoxSharedNetworkAdmission(t *testing.T) {
	a := &App{
		Store:          store.NewMemory(),
		Catalog:        upgradeChainCatalog(t),
		InfraProviders: []string{domain.ProviderKVM, domain.ProviderProxmox},
		ProviderBudgets: map[string]quota.Budget{
			domain.ProviderProxmox: {TotalVCPU: 64, TotalMemMB: 128 * 1024, TotalDiskGB: 1 << 20},
		},
		sharedNet: map[string]sharedNetSettings{
			domain.ProviderProxmox: {
				Network: "vmbr0", NetMode: "static", NetCIDR: "172.23.234.0/24",
				NetGateway: "172.23.234.254", NetDNS: "172.23.234.10",
				NetRange: "172.23.234.50-172.23.234.70",
			},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	owner := &domain.User{ID: "u1", Username: "owner", Quotas: map[string]domain.ResourceQuota{
		domain.ProviderProxmox: {VCPU: 64, MemMB: 128 * 1024, DiskGB: 8192},
	}}
	if err := a.Store.CreateUser(owner); err != nil {
		t.Fatal(err)
	}
	c, err := a.CreateCluster(owner, CreateRequest{Name: "px", Size: "small", Provider: domain.ProviderProxmox})
	if err != nil {
		t.Fatal(err)
	}
	if c.InfraProvider() != domain.ProviderProxmox {
		t.Fatalf("provider = %s, want proxmox", c.InfraProvider())
	}
	if c.NetworkName != "vmbr0" || c.IPMode != "static" || c.NetGateway != "172.23.234.254" {
		t.Fatalf("network = %s/%s/%s, want vmbr0/static/172.23.234.254", c.NetworkName, c.IPMode, c.NetGateway)
	}
	// The single control-plane node must have been allocated an address from the range.
	if len(c.StaticIPs) == 0 {
		t.Fatal("no static IP allocated for the proxmox cluster's node")
	}
	for _, ip := range c.StaticIPs {
		if !strings.HasPrefix(ip, "172.23.234.") {
			t.Fatalf("allocated IP %s is outside the configured range", ip)
		}
	}
	// A network_cidr on a shared-network provider is deployment config, not the user's to set.
	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "px2", Size: "small", Provider: domain.ProviderProxmox, NetworkCIDR: "10.0.0.0/24",
	}); err == nil {
		t.Fatal("create with network_cidr on proxmox = nil error, want a rejection")
	}
}

func TestEnabledProvidersParsing(t *testing.T) {
	t.Setenv("KAAS_INFRA_PROVIDERS", " KVM , vsphere ,kvm, proxmox ")
	got, err := enabledProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "kvm" || got[1] != "vsphere" || got[2] != "proxmox" {
		t.Fatalf("enabledProviders = %v, want [kvm vsphere proxmox] (normalized, deduped, order preserved)", got)
	}

	t.Setenv("KAAS_INFRA_PROVIDERS", "kvm,aws")
	if _, err := enabledProviders(); err == nil {
		t.Fatal("an unknown provider = nil error, want a rejection (a typo must not silently disable a provider)")
	}
}

// A provider the deployment doesn't offer must be rejected - otherwise a request could name a
// backend with no provisioner behind it, and the cluster would never converge.
func TestCreateRejectsUnknownProvider(t *testing.T) {
	a, owner := newVSphereApp(t, "dhcp")
	if _, err := a.CreateCluster(owner, CreateRequest{Name: "x", Size: "small", Provider: "proxmox"}); err == nil {
		t.Fatal("create on a disabled provider = nil error, want a rejection")
	}
	// An unspecified provider takes the deployment's default (the first enabled).
	c, err := a.CreateCluster(owner, CreateRequest{Name: "d", Size: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if c.InfraProvider() != domain.ProviderKVM {
		t.Fatalf("default provider = %s, want kvm (the first enabled)", c.InfraProvider())
	}
}

// In DHCP mode the platform doesn't allocate: an HA cluster's VIP is the user's to pick, and it
// must be a real free host on the vSphere subnet.
func TestVSphereDHCPRequiresValidUniqueVIP(t *testing.T) {
	a, owner := newVSphereApp(t, "dhcp")

	// A LoadBalancerIP is supplied throughout so these assertions exercise the VIP requirement (it is
	// itself required in dhcp mode - see TestVSphereDHCPRequiresLoadBalancerIP).
	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "ha", Size: "small", HA: true, Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.230",
	}); err == nil {
		t.Fatal("HA vsphere/dhcp cluster with no api_vip = nil error, want it required")
	}
	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "ha", Size: "small", HA: true, Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.230", APIVIP: "10.9.9.9",
	}); err == nil {
		t.Fatal("api_vip outside the node subnet = nil error, want a rejection")
	}

	c, err := a.CreateCluster(owner, CreateRequest{
		Name: "ha", Size: "small", HA: true, Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.230", APIVIP: "172.23.252.240",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.APIVIP != "172.23.252.240" || c.NetworkCIDR != "172.23.252.0/24" || c.NetworkName != "serviceVMNetwork" {
		t.Fatalf("cluster network = %+v, want the deployment's portgroup/subnet and the chosen VIP", c)
	}
	if c.LoadBalancerIP != "172.23.252.230" {
		t.Fatalf("load_balancer_ip = %q, want the chosen 172.23.252.230", c.LoadBalancerIP)
	}

	// Two clusters cannot float the same VIP on a shared subnet.
	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "ha2", Size: "small", HA: true, Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.231", APIVIP: "172.23.252.240",
	}); err == nil {
		t.Fatal("a duplicate api_vip = nil error, want a rejection (two clusters would fight over the address)")
	}

	// A single-node cluster needs no VIP, but still needs a LoadBalancer IP.
	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "single", Size: "small", Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.232",
	}); err != nil {
		t.Fatalf("single-node vsphere/dhcp cluster: %v", err)
	}
}

// In DHCP mode the platform can't allocate outside the DHCP pool, so the LoadBalancer address (for
// the default MetalLB pool / Envoy Gateway, which ship by default) is the user's to pick on EVERY
// cluster - and it must be a real, free, unique host on the shared subnet.
func TestVSphereDHCPRequiresLoadBalancerIP(t *testing.T) {
	a, owner := newVSphereApp(t, "dhcp")

	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "no-lb", Size: "small", Provider: domain.ProviderVSphere,
	}); err == nil {
		t.Fatal("vsphere/dhcp cluster with no load_balancer_ip = nil error, want it required")
	}
	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "bad-lb", Size: "small", Provider: domain.ProviderVSphere, LoadBalancerIP: "10.9.9.9",
	}); err == nil {
		t.Fatal("load_balancer_ip outside the node subnet = nil error, want a rejection")
	}
	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "ok", Size: "small", Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.235",
	}); err != nil {
		t.Fatalf("valid load_balancer_ip: %v", err)
	}
	// Two clusters cannot claim the same LoadBalancer address on a shared subnet.
	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "dup", Size: "small", Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.235",
	}); err == nil {
		t.Fatal("a duplicate load_balancer_ip = nil error, want a rejection")
	}
}

// In static mode the platform allocates every node address and the VIP, keyed by VM name so a
// re-created node keeps its IP.
func TestVSphereStaticAllocatesNodeIPs(t *testing.T) {
	a, owner := newVSphereApp(t, "static")

	c, err := a.CreateCluster(owner, CreateRequest{
		Name: "ha", Size: "small", HA: true, NodePools: pools(1), Provider: domain.ProviderVSphere,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range domain.NodeVMNames(c) {
		if c.StaticIPs[name] == "" {
			t.Fatalf("node %s has no allocated IP; got %v", name, c.StaticIPs)
		}
	}
	if c.APIVIP == "" {
		t.Fatal("HA static cluster has no VIP allocated")
	}
	if c.LoadBalancerIP == "" {
		t.Fatal("static cluster has no LoadBalancer IP allocated")
	}
	if c.NetGateway != "172.23.252.1" {
		t.Fatalf("gateway = %q, want the deployment's", c.NetGateway)
	}

	// A second cluster must not be handed any address the first holds.
	c2, err := a.CreateCluster(owner, CreateRequest{
		Name: "two", Size: "small", NodePools: pools(1), Provider: domain.ProviderVSphere,
	})
	if err != nil {
		t.Fatal(err)
	}
	taken := map[string]bool{c.APIVIP: true, c.LoadBalancerIP: true}
	if c2.LoadBalancerIP == c.LoadBalancerIP {
		t.Fatalf("cluster two got LoadBalancer IP %s, which cluster ha already holds", c2.LoadBalancerIP)
	}
	for _, ip := range c.StaticIPs {
		taken[ip] = true
	}
	for name, ip := range c2.StaticIPs {
		if taken[ip] {
			t.Fatalf("cluster two's node %s got %s, which cluster ha already holds", name, ip)
		}
	}
}

// Scaling a static cluster must extend the allocation to the new workers and keep every existing
// node on the address it already has (the stable-IP contract).
func TestVSphereStaticScalePreservesIPs(t *testing.T) {
	a, owner := newVSphereApp(t, "static")
	c, err := a.CreateCluster(owner, CreateRequest{
		Name: "s", Size: "small", NodePools: pools(1), Provider: domain.ProviderVSphere,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for k, v := range c.StaticIPs {
		before[k] = v
	}

	up, err := a.UpdateCluster(owner, c.ID, UpdateRequest{NodePools: ptr(pools(2))})
	if err != nil {
		t.Fatal(err)
	}
	for name, ip := range before {
		if up.StaticIPs[name] != ip {
			t.Fatalf("node %s moved from %s to %s on scale-up - a re-created node would lose its address",
				name, ip, up.StaticIPs[name])
		}
	}
	if up.StaticIPs["s-default-1"] == "" {
		t.Fatalf("the new worker got no address; got %v", up.StaticIPs)
	}

	down, err := a.UpdateCluster(owner, c.ID, UpdateRequest{NodePools: ptr(pools(0))})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := down.StaticIPs["s-default-0"]; ok {
		t.Fatalf("scale-down left a removed worker's address allocated: %v", down.StaticIPs)
	}
	if down.StaticIPs["s-cp-0"] != before["s-cp-0"] {
		t.Error("scale-down moved the control plane's address")
	}
}

// Per-cluster CIDR exclusivity is a KVM invariant: vSphere clusters all share the operator's
// subnet, so it must not be treated as "taken" by the KVM allocator, and two vSphere clusters
// sharing it must be fine.
func TestVSphereClustersShareTheSubnet(t *testing.T) {
	a, owner := newVSphereApp(t, "dhcp")
	lbIPs := map[string]string{"a": "172.23.252.230", "b": "172.23.252.231"} // dhcp mode: user picks the LB address
	for _, name := range []string{"a", "b"} {
		c, err := a.CreateCluster(owner, CreateRequest{
			Name: name, Size: "small", Provider: domain.ProviderVSphere, LoadBalancerIP: lbIPs[name],
		})
		if err != nil {
			t.Fatalf("vsphere cluster %s: %v", name, err)
		}
		if c.NetworkCIDR != "172.23.252.0/24" {
			t.Fatalf("cluster %s CIDR = %s, want the shared portgroup subnet", name, c.NetworkCIDR)
		}
	}
	// A KVM cluster still gets its own auto-allocated block, unaffected by the vSphere clusters.
	k, err := a.CreateCluster(owner, CreateRequest{Name: "k", Size: "small", Provider: domain.ProviderKVM})
	if err != nil {
		t.Fatal(err)
	}
	if k.NetworkCIDR == "172.23.252.0/24" || k.NetworkCIDR == "" {
		t.Fatalf("kvm cluster CIDR = %q, want its own block from the platform supernet", k.NetworkCIDR)
	}
}

// A user-supplied CIDR is meaningless on vSphere (the subnet is the operator's), so it must be
// rejected rather than silently ignored.
func TestVSphereRejectsRequestedCIDR(t *testing.T) {
	a, owner := newVSphereApp(t, "dhcp")
	if _, err := a.CreateCluster(owner, CreateRequest{
		Name: "x", Size: "small", Provider: domain.ProviderVSphere, NetworkCIDR: "10.20.0.0/24",
	}); err == nil {
		t.Fatal("network_cidr on vsphere = nil error, want a rejection")
	}
}

// A provider's platform-wide ceiling protects that ONE backend from being oversubscribed even when
// the owner's personal quota still has room - the owner's quota is a single pool spanning every
// provider, so it can't do this job on its own.
func TestVSphereCapacityCap(t *testing.T) {
	a, owner := newVSphereApp(t, "dhcp")
	a.ProviderBudgets[domain.ProviderVSphere] = quota.Budget{TotalVCPU: 4, TotalMemMB: 8192, TotalDiskGB: 1 << 20} // exactly one small node

	if _, err := a.CreateCluster(owner, CreateRequest{Name: "one", Size: "small", Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.230"}); err != nil {
		t.Fatalf("first vsphere cluster within the cap: %v", err)
	}
	if _, err := a.CreateCluster(owner, CreateRequest{Name: "two", Size: "small", Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.231"}); err == nil {
		t.Fatal("a second vsphere cluster over the platform cap = nil error, want a rejection")
	}
	// The cap is vSphere's alone - a KVM cluster is unaffected by it.
	if _, err := a.CreateCluster(owner, CreateRequest{Name: "k", Size: "small", Provider: domain.ProviderKVM}); err != nil {
		t.Fatalf("kvm cluster rejected by the vsphere cap: %v", err)
	}
}

// The KVM host has a ceiling of its own, and it has to be enforced for the same reason vSphere's
// is: the tenant pool is the SUM of both infrastructures, so a tenant granted the whole pool could
// otherwise spend capacity that only vSphere can fund on the KVM host.
func TestKVMCapacityCap(t *testing.T) {
	a, owner := newVSphereApp(t, "dhcp")
	a.ProviderBudgets[domain.ProviderKVM] = quota.Budget{TotalVCPU: 4, TotalMemMB: 8192, TotalDiskGB: 1 << 20} // one small node

	if _, err := a.CreateCluster(owner, CreateRequest{Name: "one", Size: "small", Provider: domain.ProviderKVM}); err != nil {
		t.Fatalf("first kvm cluster within the host ceiling: %v", err)
	}
	if _, err := a.CreateCluster(owner, CreateRequest{Name: "two", Size: "small", Provider: domain.ProviderKVM}); err == nil {
		t.Fatal("a second kvm cluster over the host ceiling = nil error, want a rejection - the owner's quota is a cross-provider pool and cannot protect the host")
	}
	if _, err := a.CreateCluster(owner, CreateRequest{Name: "v", Size: "small", Provider: domain.ProviderVSphere, LoadBalancerIP: "172.23.252.230"}); err != nil {
		t.Fatalf("vsphere cluster rejected by the kvm host ceiling: %v", err)
	}
}

// Each enabled infrastructure gets its own ceiling from its own env vars, and a provider that
// isn't enabled contributes no capacity at all.
func TestPlatformCapacityPerProvider(t *testing.T) {
	t.Setenv("KAAS_BUDGET_VCPU", "16")
	t.Setenv("KAAS_BUDGET_MEM_MB", "24576")
	t.Setenv("KAAS_BUDGET_DISK_GB", "500")
	shared := map[string]sharedNetSettings{
		domain.ProviderVSphere: {Budget: quota.Budget{TotalVCPU: 64, TotalMemMB: 131072, TotalDiskGB: 4096}},
		domain.ProviderProxmox: {Budget: quota.Budget{TotalVCPU: 32, TotalMemMB: 65536, TotalDiskGB: 2048}},
	}

	per := platformCapacity([]string{domain.ProviderKVM, domain.ProviderVSphere, domain.ProviderProxmox}, shared)
	if per[domain.ProviderKVM] != (quota.Budget{TotalVCPU: 16, TotalMemMB: 24576, TotalDiskGB: 500}) {
		t.Fatalf("kvm ceiling = %+v, want the KAAS_BUDGET_* values", per[domain.ProviderKVM])
	}
	if per[domain.ProviderVSphere] != (quota.Budget{TotalVCPU: 64, TotalMemMB: 131072, TotalDiskGB: 4096}) {
		t.Fatalf("vsphere ceiling = %+v, want the vsphere budget", per[domain.ProviderVSphere])
	}
	if per[domain.ProviderProxmox] != (quota.Budget{TotalVCPU: 32, TotalMemMB: 65536, TotalDiskGB: 2048}) {
		t.Fatalf("proxmox ceiling = %+v, want the proxmox budget", per[domain.ProviderProxmox])
	}

	if kvmOnly := platformCapacity([]string{domain.ProviderKVM}, shared); len(kvmOnly) != 1 {
		t.Fatalf("kvm-only deployment has ceilings %v, want kvm alone (a disabled backend has no capacity here)", kvmOnly)
	}
}
