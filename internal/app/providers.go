package app

// Multi-provider support: KAAS_INFRA_PROVIDERS lists the infrastructure providers this
// deployment offers (kvm, vsphere, proxmox). It is orthogonal to KAAS_PROVISIONER, which stays the
// fake/real seam switch - in fake mode every enabled provider maps to the shared fake
// provisioner so the whole flow (wizard step, provider badge, reconcile) demos without
// vCenter, KVM or Proxmox. The provider a cluster was created on is immutable desired spec
// (domain.Cluster.Provider); everything provider-shaped below is deployment-level
// configuration copied onto the cluster at admission.
//
// vSphere and Proxmox are the same KIND of provider - VMs clone a golden template onto the
// operator's SHARED network (a portgroup / a bridge), and node addressing is either external DHCP
// (read back from the guest agent) or platform-allocated static. So they share one deployment-level
// config shape (sharedNetSettings) and one admission path (admitSharedNetwork / scaleSharedStaticIPs).
// KVM is the odd one out: each cluster gets its own dedicated per-cluster network.

import (
	"fmt"
	"net"
	"os"
	"slices"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/netpool"
	"github.com/Daniel-Vaz/KaaS-demo/internal/quota"
)

// ProviderInfo is what the portal needs to render the wizard's Infrastructure step (shown only
// when more than one provider is enabled) and a provider-aware networking step. Served on the
// catalog payload (GET /catalog).
type ProviderInfo struct {
	Name string `json:"name"`
	// Shared-network providers (vsphere, proxmox) only: the deployment's network shape, so the
	// wizard can show where nodes will land and whether to ask for an HA control-plane VIP (ip_mode
	// "dhcp" does; "static" is allocated by the platform). Empty on kvm, whose per-cluster network
	// is chosen at create time.
	IPMode      string `json:"ip_mode,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
	NetworkCIDR string `json:"network_cidr,omitempty"`
	// NetRange is the static-allocation range ("from-to") node addresses and the HA VIP are drawn
	// from. Only meaningful (and only set) when IPMode is "static".
	NetRange string `json:"net_range,omitempty"`
}

// platformCapacity is the ceiling of each enabled infrastructure, keyed by provider: KVM's is the
// host this control plane runs on (KAAS_BUDGET_*), the shared-network providers' come from their
// own deployment settings (KAAS_VSPHERE_BUDGET_* / KAAS_PROXMOX_BUDGET_*).
//
// There is deliberately no summed "platform total" here. Quota is granted and charged on ONE
// infrastructure at a time (see the quota package doc) because capacity is not fungible between
// backends - the KVM host's spare cores cannot run a vSphere VM, nor a vSphere core a Proxmox VM -
// so a single pooled number would be a figure nothing could be admitted against.
func platformCapacity(providers []string, shared map[string]sharedNetSettings) map[string]quota.Budget {
	per := make(map[string]quota.Budget, len(providers))
	for _, p := range providers {
		if s, ok := shared[p]; ok { // a shared-network provider carries its own budget
			per[p] = s.Budget
			continue
		}
		// kvm - the host this control plane runs on.
		per[p] = quota.Budget{
			TotalVCPU:  envInt("KAAS_BUDGET_VCPU", 16),
			TotalMemMB: envInt("KAAS_BUDGET_MEM_MB", 24576),
			// The libvirt storage pool's share this platform may hand out. Default 500 GB: about
			// ten default-sized (50 GB) nodes with room for a few extra disks, which is the
			// most a laptop's pool sensibly carries.
			TotalDiskGB: envInt("KAAS_BUDGET_DISK_GB", 500),
		}
	}
	return per
}

// infraProviders is the enabled provider list, defaulting to kvm alone for an App built without
// one (tests, and any caller that never touched the env).
func (a *App) infraProviders() []string {
	if len(a.InfraProviders) == 0 {
		return []string{domain.ProviderKVM}
	}
	return a.InfraProviders
}

// Providers reports the enabled infrastructure providers in configured order (first = default).
func (a *App) Providers() []ProviderInfo {
	names := a.infraProviders()
	out := make([]ProviderInfo, 0, len(names))
	for _, name := range names {
		info := ProviderInfo{Name: name}
		if s, ok := a.sharedNet[name]; ok {
			info.IPMode = s.NetMode
			info.NetworkName = s.Network
			info.NetworkCIDR = s.NetCIDR
			if s.NetMode == "static" {
				info.NetRange = s.NetRange
			}
		}
		out = append(out, info)
	}
	return out
}

// enabledProviders parses KAAS_INFRA_PROVIDERS (default "kvm"). Order is meaningful: the first
// entry is the default provider for create requests that don't name one.
func enabledProviders() ([]string, error) {
	spec := getenv("KAAS_INFRA_PROVIDERS", domain.ProviderKVM)
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(spec, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		switch p {
		case domain.ProviderKVM, domain.ProviderVSphere, domain.ProviderProxmox:
		default:
			return nil, fmt.Errorf("KAAS_INFRA_PROVIDERS entry %q unknown (want kvm|vsphere|proxmox)", p)
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("KAAS_INFRA_PROVIDERS is empty")
	}
	return out, nil
}

// sharedNetSettings is the deployment-level network + capacity configuration for a shared-network
// provider (vSphere or Proxmox), parsed once in New. Placement/credentials (vCenter URL / Proxmox
// endpoint, datacenter/node, datastore, …) are the worker's concern (see buildProvisioners);
// admission only needs the network shape and the capacity budget, so the API container never sees
// backend credentials.
type sharedNetSettings struct {
	Network    string // vsphere: portgroup name; proxmox: bridge name
	NetMode    string // "dhcp" | "static"
	NetCIDR    string // the shared subnet, e.g. 172.23.252.0/24
	NetGateway string // static mode
	NetDNS     string // static mode, comma-separated
	NetRange   string // static mode allocation range "from-to"
	// Platform-wide capacity ceiling for this infrastructure (its own conserved pool - quota is
	// never fungible across backends). Production would carve per-user, per-provider quotas.
	Budget quota.Budget
}

func vsphereFromEnv() (sharedNetSettings, error) {
	s := sharedNetSettings{
		Network:    os.Getenv("KAAS_VSPHERE_NETWORK"),
		NetMode:    strings.ToLower(getenv("KAAS_VSPHERE_NET_MODE", "dhcp")),
		NetCIDR:    os.Getenv("KAAS_VSPHERE_NET_CIDR"),
		NetGateway: os.Getenv("KAAS_VSPHERE_NET_GATEWAY"),
		NetDNS:     os.Getenv("KAAS_VSPHERE_NET_DNS"),
		NetRange:   os.Getenv("KAAS_VSPHERE_NET_RANGE"),
		Budget: quota.Budget{
			TotalVCPU:   envInt("KAAS_VSPHERE_BUDGET_VCPU", 64),
			TotalMemMB:  envInt("KAAS_VSPHERE_BUDGET_MEM_MB", 131072),
			TotalDiskGB: envInt("KAAS_VSPHERE_BUDGET_DISK_GB", 4096),
		},
	}
	if s.Network == "" {
		return s, fmt.Errorf("KAAS_VSPHERE_NETWORK is required when the vsphere provider is enabled")
	}
	return s, validateSharedNet(s, "KAAS_VSPHERE")
}

func proxmoxFromEnv() (sharedNetSettings, error) {
	s := sharedNetSettings{
		Network:    os.Getenv("KAAS_PROXMOX_NET_BRIDGE"),
		NetMode:    strings.ToLower(getenv("KAAS_PROXMOX_NET_MODE", "dhcp")),
		NetCIDR:    os.Getenv("KAAS_PROXMOX_NET_CIDR"),
		NetGateway: os.Getenv("KAAS_PROXMOX_NET_GATEWAY"),
		NetDNS:     os.Getenv("KAAS_PROXMOX_NET_DNS"),
		NetRange:   os.Getenv("KAAS_PROXMOX_NET_RANGE"),
		Budget: quota.Budget{
			TotalVCPU:   envInt("KAAS_PROXMOX_BUDGET_VCPU", 64),
			TotalMemMB:  envInt("KAAS_PROXMOX_BUDGET_MEM_MB", 131072),
			TotalDiskGB: envInt("KAAS_PROXMOX_BUDGET_DISK_GB", 4096),
		},
	}
	if s.Network == "" {
		return s, fmt.Errorf("KAAS_PROXMOX_NET_BRIDGE is required when the proxmox provider is enabled")
	}
	return s, validateSharedNet(s, "KAAS_PROXMOX")
}

// validateSharedNet checks the CIDR and, in static mode, the gateway/DNS/range of a shared-network
// provider's config. prefix names the env family in error messages ("KAAS_VSPHERE"/"KAAS_PROXMOX").
func validateSharedNet(s sharedNetSettings, prefix string) error {
	if s.NetCIDR == "" {
		return fmt.Errorf("%s_NET_CIDR is required", prefix)
	}
	if _, _, err := net.ParseCIDR(s.NetCIDR); err != nil {
		return fmt.Errorf("%s_NET_CIDR: %w", prefix, err)
	}
	switch s.NetMode {
	case "dhcp":
	case "static":
		if s.NetGateway == "" || s.NetRange == "" || s.NetDNS == "" {
			return fmt.Errorf("%s_NET_MODE=static requires %s_NET_GATEWAY, %s_NET_DNS and %s_NET_RANGE", prefix, prefix, prefix, prefix)
		}
		if net.ParseIP(s.NetGateway) == nil {
			return fmt.Errorf("%s_NET_GATEWAY %q is not a valid IP", prefix, s.NetGateway)
		}
		if _, _, err := netpool.ParseRange(s.NetRange); err != nil {
			return fmt.Errorf("%s_NET_RANGE: %w", prefix, err)
		}
	default:
		return fmt.Errorf("%s_NET_MODE %q unknown (want dhcp|static)", prefix, s.NetMode)
	}
	return nil
}

// resolveProvider validates a create request's provider against the enabled set, defaulting an
// empty one to the first (only, in most deployments) enabled provider.
func (a *App) resolveProvider(requested string) (string, error) {
	enabled := a.infraProviders()
	p := strings.ToLower(strings.TrimSpace(requested))
	if p == "" {
		return enabled[0], nil
	}
	if slices.Contains(enabled, p) {
		return p, nil
	}
	return "", fmt.Errorf("infrastructure provider %q is not enabled (available: %s)", requested, strings.Join(enabled, ", "))
}

// clustersOnProvider filters to the live clusters on one provider - the scope for provider-wide
// resources (kvm: per-cluster CIDR exclusivity; vsphere/proxmox: shared-subnet IPs and the capacity cap).
func clustersOnProvider(clusters []*domain.Cluster, provider string) []*domain.Cluster {
	var out []*domain.Cluster
	for _, c := range clusters {
		if c.InfraProvider() == provider {
			out = append(out, c)
		}
	}
	return out
}

// admitSharedNetwork is the shared-network arm of admission (vSphere and Proxmox): it stamps the
// deployment's network shape onto the cluster and resolves node/VIP addressing. dhcp mode records
// the user-supplied HA VIP (validated in-subnet and unique among live clusters on the SAME provider
// - the external DHCP server owns node IPs). static mode allocates node IPs and the HA VIP from the
// operator's range, keyed by vm_name so a re-created node keeps its address. providerClusters must
// be the live clusters on c's provider. Callers MUST hold store.LockAdmission: the free-set
// computation reads every live cluster on the provider.
func (a *App) admitSharedNetwork(c *domain.Cluster, s sharedNetSettings, requestedVIP, requestedLB string, providerClusters []*domain.Cluster) error {
	c.NetworkCIDR = s.NetCIDR
	c.NetworkName = s.Network
	c.IPMode = s.NetMode
	used := map[string]bool{}
	for _, other := range providerClusters {
		if other.DeletedAt != nil {
			continue
		}
		for _, ip := range other.StaticIPs {
			used[ip] = true
		}
		if other.APIVIP != "" {
			used[other.APIVIP] = true
		}
		if other.LoadBalancerIP != "" {
			used[other.LoadBalancerIP] = true
		}
	}
	switch s.NetMode {
	case "dhcp":
		// The external DHCP server owns node addressing, so the platform can't allocate the VIP or the
		// LoadBalancer address - the user names a free host outside the pool for each. The VIP is only
		// needed for HA; the LoadBalancer IP is needed on every cluster (metallb/envoy ship by default).
		lb := strings.TrimSpace(requestedLB)
		if lb == "" {
			return fmt.Errorf("load_balancer_ip is required on %s (dhcp mode): pick a free address in %s outside the DHCP pool for the default MetalLB pool / Envoy Gateway", c.InfraProvider(), s.NetCIDR)
		}
		if err := a.validateSharedHostIP(s, "load_balancer_ip", lb, used); err != nil {
			return err
		}
		c.LoadBalancerIP = lb
		used[lb] = true
		if c.HA() {
			vip := strings.TrimSpace(requestedVIP)
			if vip == "" {
				return fmt.Errorf("api_vip is required for an HA control plane on %s (dhcp mode): pick a free address in %s outside the DHCP pool", c.InfraProvider(), s.NetCIDR)
			}
			if err := a.validateSharedHostIP(s, "api_vip", vip, used); err != nil {
				return err
			}
			c.APIVIP = vip
		}
		return nil
	case "static":
		c.NetGateway = s.NetGateway
		c.NetDNS = s.NetDNS
		names := domain.NodeVMNames(c)
		n := len(names)
		n++ // the LoadBalancer IP comes from the same range (metallb/envoy ship by default)
		if c.HA() {
			n++ // as does the VIP
		}
		ips, err := netpool.AllocateStatic(s.NetRange, s.NetCIDR, s.NetGateway, used, n)
		if err != nil {
			return err
		}
		c.StaticIPs = make(map[string]string, len(names))
		for i, name := range names {
			c.StaticIPs[name] = ips[i]
		}
		next := len(names)
		c.LoadBalancerIP = ips[next]
		next++
		if c.HA() {
			c.APIVIP = ips[next]
		}
		return nil
	default:
		return fmt.Errorf("cluster %s has unknown ip_mode %q", c.Name, s.NetMode)
	}
}

// validateSharedHostIP checks a user-supplied address on a shared subnet: a usable host in the CIDR,
// not the gateway, and not already claimed by another cluster (or an earlier field on this one).
// field names the request field for the error ("api_vip" / "load_balancer_ip").
func (a *App) validateSharedHostIP(s sharedNetSettings, field, ip string, used map[string]bool) error {
	ok, err := netpool.ContainsIP(s.NetCIDR, ip)
	if err != nil {
		return err
	}
	if !ok || ip == s.NetGateway {
		return fmt.Errorf("%s %s is not a usable host address in %s", field, ip, s.NetCIDR)
	}
	if used[ip] {
		return fmt.Errorf("%s %s is already in use by another cluster", field, ip)
	}
	return nil
}

// scaleSharedStaticIPs re-converges a static-mode shared-network cluster's per-node IP allocation to
// c's CURRENT desired shape: names that are newly desired get addresses from the range, names no
// longer desired release theirs. Existing allocations are never moved, so a node that survives the
// edit keeps its address - the stable-IP-on-recreate contract.
//
// c must already carry the desired node pools (callers pass the candidate, not the stored cluster),
// and it is mutated in place: c.StaticIPs is replaced. Callers MUST hold store.LockAdmission.
//
// Adding, scaling and deleting a pool all reduce to the same thing here, because a node's pool is
// part of its VM name and the allocation is keyed on that name.
func (a *App) scaleSharedStaticIPs(c *domain.Cluster) error {
	s, ok := a.sharedNet[c.InfraProvider()]
	if !ok || c.IPMode != "static" {
		return nil
	}
	all, err := a.Store.ListClusters()
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for _, other := range clustersOnProvider(all, c.InfraProvider()) {
		if other.ID == c.ID || other.DeletedAt != nil {
			continue
		}
		for _, ip := range other.StaticIPs {
			used[ip] = true
		}
		if other.APIVIP != "" {
			used[other.APIVIP] = true
		}
		if other.LoadBalancerIP != "" {
			used[other.LoadBalancerIP] = true
		}
	}
	// The VIP and the LoadBalancer IP are fixed per-cluster addresses, not per-node, so they are not
	// recomputed here - but they must stay reserved so a newly-desired node never reuses them.
	if c.APIVIP != "" {
		used[c.APIVIP] = true
	}
	if c.LoadBalancerIP != "" {
		used[c.LoadBalancerIP] = true
	}
	desired := domain.NodeVMNames(c)
	next := make(map[string]string, len(desired))
	var missing []string
	for _, name := range desired {
		if ip, ok := c.StaticIPs[name]; ok {
			next[name] = ip
			used[ip] = true
		} else {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		ips, err := netpool.AllocateStatic(s.NetRange, s.NetCIDR, s.NetGateway, used, len(missing))
		if err != nil {
			return err
		}
		for i, name := range missing {
			next[name] = ips[i]
		}
	}
	c.StaticIPs = next
	return nil
}

// checkProviderCapacity is the platform-wide admission cap for ONE infrastructure (see
// platformCapacity), checked IN ADDITION to the owner's personal quota - the owner's quota is a
// conserved pool spanning every provider, so it cannot on its own stop one backend being
// oversubscribed. existing must be the live clusters on that provider, excluding the one being
// admitted/resized. Callers MUST hold store.LockAdmission.
//
// A provider with no configured ceiling (an App built without New - tests) is uncapped: the
// owner's quota stays the only gate, exactly as before per-provider ceilings existed.
func (a *App) checkProviderCapacity(provider string, existing []*domain.Cluster, want *domain.Cluster) error {
	b, ok := a.ProviderBudgets[provider]
	if !ok {
		return nil
	}
	if err := b.Check(existing, want); err != nil {
		return fmt.Errorf("%s capacity: %w", provider, err)
	}
	return nil
}
