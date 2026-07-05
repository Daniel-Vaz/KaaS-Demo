// Package netpool is the IP-address-management (IPAM) admission check for a cluster's dedicated
// node network. Each cluster provisions its own isolated libvirt NAT bridge (see infra/libvirt and
// docs/networking.md); this package decides the CIDR that bridge uses - either validating a
// user-supplied one or auto-allocating the next free block from the platform supernet - and
// derives the HA API VIP from it.
//
// It is a first-class admission check like internal/quota: a CIDR that overlaps the Kubernetes
// pod/service networks, Podman's bridges, the libvirt default network, or another live cluster
// would silently break routing, so those overlaps are rejected at create time.
package netpool

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

const (
	defaultSupernet = "10.200.0.0/16" // carved into per-cluster blocks; clear of pod/svc/podman/libvirt ranges
	defaultPrefix   = 24              // size of an auto-allocated cluster network
	minPrefix       = 16              // biggest a cluster network may be
	maxPrefix       = 28              // smallest (a /28 still fits 3 control planes + a few workers + the VIP)

	// vipOffsetFromTop places the HA API VIP this far below the subnet broadcast - high in the
	// range, above the DHCP pool libvirt's dnsmasq fills from the bottom (see docs/networking.md).
	vipOffsetFromTop = 6

	// lbOffsetFromTop places the default MetalLB LoadBalancer address one slot below the VIP - also
	// high in the range, above the bottom-filled DHCP pool. One more than vipOffsetFromTop so it can
	// never land on the VIP in either the normal or the tiny-subnet fallback branch (see VIP).
	lbOffsetFromTop = 7
)

// defaultReserved are ranges a cluster network must never overlap: the Kubernetes pod/service
// CIDRs (baked in internal/app), Podman/netavark's bridges, and the libvirt default network.
// Overriding via KAAS_NET_RESERVED replaces this list wholesale.
var defaultReserved = []string{
	"10.244.0.0/16",    // pod CIDR (Cilium) - internal/app
	"10.96.0.0/12",     // service CIDR - internal/app
	"10.88.0.0/16",     // Podman/netavark default bridge
	"10.89.0.0/16",     // Podman/netavark additional bridges
	"192.168.122.0/24", // libvirt "default" network (virbr0)
}

// Allocate resolves the node-network CIDR for a new cluster. A non-empty requested CIDR is
// validated and returned canonicalised (as its network address); an empty one is auto-allocated
// as the next free block from the platform supernet. In both cases the result is guaranteed clear
// of the reserved ranges and of every live cluster's network.
func Allocate(existing []*domain.Cluster, requested string) (string, error) {
	reserved, err := reservedNets()
	if err != nil {
		return "", err
	}
	used := usedNets(existing)
	if strings.TrimSpace(requested) != "" {
		return validateRequested(requested, reserved, used)
	}
	return autoAllocate(reserved, used)
}

func validateRequested(requested string, reserved, used []*net.IPNet) (string, error) {
	_, ipnet, err := net.ParseCIDR(strings.TrimSpace(requested))
	if err != nil {
		return "", fmt.Errorf("network_cidr %q is not a valid CIDR: %w", requested, err)
	}
	if ipnet.IP.To4() == nil {
		return "", fmt.Errorf("network_cidr %q must be IPv4", requested)
	}
	ones, _ := ipnet.Mask.Size()
	if ones < minPrefix || ones > maxPrefix {
		return "", fmt.Errorf("network_cidr prefix /%d out of range - must be between /%d and /%d", ones, minPrefix, maxPrefix)
	}
	if n := overlapName(ipnet, reserved); n != "" {
		return "", fmt.Errorf("network_cidr %s overlaps a reserved range (%s) - pick a different block", ipnet.String(), n)
	}
	if n := overlapName(ipnet, used); n != "" {
		return "", fmt.Errorf("network_cidr %s overlaps another cluster's network (%s)", ipnet.String(), n)
	}
	return ipnet.String(), nil
}

func autoAllocate(reserved, used []*net.IPNet) (string, error) {
	_, super, err := net.ParseCIDR(getenv("KAAS_NET_SUPERNET", defaultSupernet))
	if err != nil {
		return "", fmt.Errorf("KAAS_NET_SUPERNET invalid: %w", err)
	}
	if super.IP.To4() == nil {
		return "", fmt.Errorf("KAAS_NET_SUPERNET %s must be IPv4", super)
	}
	prefix := envInt("KAAS_NET_PREFIX", defaultPrefix)
	superOnes, bits := super.Mask.Size()
	if prefix < superOnes || prefix > maxPrefix {
		return "", fmt.Errorf("KAAS_NET_PREFIX /%d invalid for supernet %s (must be between /%d and /%d)", prefix, super, superOnes, maxPrefix)
	}
	start := binary.BigEndian.Uint32(super.IP.To4())
	superSize := uint32(1) << uint(bits-superOnes)
	step := uint32(1) << uint(bits-prefix)
	for addr := start; addr < start+superSize && addr >= start; addr += step {
		cand := &net.IPNet{IP: uint32ToIP(addr), Mask: net.CIDRMask(prefix, bits)}
		if overlaps(cand, reserved) || overlaps(cand, used) {
			continue
		}
		return cand.String(), nil
	}
	return "", fmt.Errorf("no free /%d network available in supernet %s (too many clusters)", prefix, super)
}

// VIP derives the HA control-plane API VIP for a cluster from its node-network CIDR: a fixed high
// host in the subnet, above the DHCP pool. Because each cluster owns an isolated subnet there is
// no cross-cluster collision, so the address is deterministic (no pool needed).
func VIP(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("network %q: %w", cidr, err)
	}
	if ipnet.IP.To4() == nil {
		return "", fmt.Errorf("network %q is not IPv4", cidr)
	}
	network := binary.BigEndian.Uint32(ipnet.IP.To4())
	ones, bits := ipnet.Mask.Size()
	size := uint32(1) << uint(bits-ones)
	off := uint32(vipOffsetFromTop)
	if size < off+3 { // tiny subnet: fall back to the last usable host
		off = 2
	}
	return uint32ToIP(network + size - off).String(), nil
}

// LoadBalancerIP derives the single node-network address reserved for a KVM cluster's default
// MetalLB L2 pool - a fixed high host one slot below the HA API VIP, above the DHCP pool. Because
// each KVM cluster owns an isolated subnet there is no cross-cluster collision, so it is
// deterministic (no pool needed), the same reasoning as VIP. Shared-network providers allocate it
// from the operator's range or take it from the user instead (see internal/app.admitSharedNetwork).
func LoadBalancerIP(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("network %q: %w", cidr, err)
	}
	if ipnet.IP.To4() == nil {
		return "", fmt.Errorf("network %q is not IPv4", cidr)
	}
	network := binary.BigEndian.Uint32(ipnet.IP.To4())
	ones, bits := ipnet.Mask.Size()
	size := uint32(1) << uint(bits-ones)
	off := uint32(lbOffsetFromTop)
	if size < off+3 { // tiny subnet: sit just below VIP's last-usable-host fallback
		off = 3
	}
	return uint32ToIP(network + size - off).String(), nil
}

// Gateway is the address libvirt assigns to the bridge for a CIDR (the first usable host),
// exposed so callers/UI can show it without re-deriving the convention.
func Gateway(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("network %q: %w", cidr, err)
	}
	if ipnet.IP.To4() == nil {
		return "", fmt.Errorf("network %q is not IPv4", cidr)
	}
	return uint32ToIP(binary.BigEndian.Uint32(ipnet.IP.To4()) + 1).String(), nil
}

func overlaps(n *net.IPNet, list []*net.IPNet) bool { return overlapName(n, list) != "" }

func overlapName(n *net.IPNet, list []*net.IPNet) string {
	for _, o := range list {
		if n.Contains(o.IP) || o.Contains(n.IP) {
			return o.String()
		}
	}
	return ""
}

func usedNets(existing []*domain.Cluster) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range existing {
		if c.DeletedAt != nil || c.NetworkCIDR == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(c.NetworkCIDR); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func reservedNets() ([]*net.IPNet, error) {
	spec := getenv("KAAS_NET_RESERVED", strings.Join(defaultReserved, ","))
	var out []*net.IPNet
	for _, s := range strings.Split(spec, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("KAAS_NET_RESERVED entry %q invalid: %w", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func uint32ToIP(v uint32) net.IP {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return net.IP(b[:])
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
