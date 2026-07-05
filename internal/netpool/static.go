package netpool

// Static-range IPAM for providers whose clusters share one operator-owned subnet (vSphere).
// Unlike the per-cluster-CIDR model above, every cluster draws node IPs and its HA VIP from a
// single allocation range, so the free set must be computed against all live clusters on that
// network. Callers do that read-then-write under store.LockAdmission, same as Allocate; the
// results are persisted on the cluster (StaticIPs/APIVIP) keyed by vm_name, which is what keeps
// a re-created node on the same address.

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

// ParseRange parses an inclusive IPv4 allocation range "172.23.252.50-172.23.252.99".
func ParseRange(spec string) (from, to net.IP, err error) {
	parts := strings.SplitN(strings.TrimSpace(spec), "-", 2)
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("ip range %q must be \"<from>-<to>\"", spec)
	}
	from = net.ParseIP(strings.TrimSpace(parts[0]))
	to = net.ParseIP(strings.TrimSpace(parts[1]))
	if from == nil || from.To4() == nil || to == nil || to.To4() == nil {
		return nil, nil, fmt.Errorf("ip range %q must be two IPv4 addresses", spec)
	}
	if ipToUint32(from) > ipToUint32(to) {
		return nil, nil, fmt.Errorf("ip range %q is inverted (from > to)", spec)
	}
	return from, to, nil
}

// ContainsIP reports whether ip is a usable host address inside cidr - in the subnet and not
// its network, broadcast, or gateway-by-convention (.1 is NOT excluded here; pass the real
// gateway to AllocateStatic / check it at the caller, since on an operator subnet the gateway
// is configuration, not convention).
func ContainsIP(cidr, ip string) (bool, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false, fmt.Errorf("network %q: %w", cidr, err)
	}
	addr := net.ParseIP(strings.TrimSpace(ip))
	if addr == nil || addr.To4() == nil {
		return false, fmt.Errorf("ip %q is not a valid IPv4 address", ip)
	}
	if !ipnet.Contains(addr) {
		return false, nil
	}
	network := binary.BigEndian.Uint32(ipnet.IP.To4())
	ones, bits := ipnet.Mask.Size()
	size := uint32(1) << uint(bits-ones)
	v := ipToUint32(addr)
	if v == network || v == network+size-1 { // network / broadcast
		return false, nil
	}
	return true, nil
}

// AllocateStatic returns n free addresses from the allocation range, lowest first -
// deterministic, so a retried admission converges. Excluded: anything in used (other live
// clusters' node IPs and VIPs), the gateway, and the subnet's network/broadcast addresses.
func AllocateStatic(rangeSpec, cidr, gateway string, used map[string]bool, n int) ([]string, error) {
	from, to, err := ParseRange(rangeSpec)
	if err != nil {
		return nil, err
	}
	if ok, err := ContainsIP(cidr, from.String()); err != nil || !ok {
		return nil, fmt.Errorf("ip range start %s is not a usable host in %s", from, cidr)
	}
	if ok, err := ContainsIP(cidr, to.String()); err != nil || !ok {
		return nil, fmt.Errorf("ip range end %s is not a usable host in %s", to, cidr)
	}
	out := make([]string, 0, n)
	for v := ipToUint32(from); v <= ipToUint32(to) && len(out) < n; v++ {
		ip := uint32ToIP(v).String()
		if used[ip] || ip == gateway {
			continue
		}
		out = append(out, ip)
	}
	if len(out) < n {
		return nil, fmt.Errorf("ip range %s exhausted: need %d addresses, only %d free", rangeSpec, n, len(out))
	}
	return out, nil
}

func ipToUint32(ip net.IP) uint32 { return binary.BigEndian.Uint32(ip.To4()) }
