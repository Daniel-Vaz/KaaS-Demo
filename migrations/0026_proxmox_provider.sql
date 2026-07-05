-- 0026_proxmox_provider.sql - Proxmox VE as a third infrastructure provider, alongside kvm and
-- vSphere.
--
-- No new columns: Proxmox is a shared-network, clone-a-template provider exactly like vSphere, so
-- it reuses the provider + network_name/ip_mode/net_gateway/net_dns/static_ips columns that
-- 0019_vsphere_provider added. The provider column just gains a third legal value ('proxmox').
--
-- Like vSphere, Proxmox clusters all sit on the operator's shared subnet (a bridge, not a
-- portgroup), so per-cluster CIDR exclusivity stays a kvm-only invariant (the 0019 index is already
-- scoped to provider='kvm') and the per-cluster-exclusive resource is the HA API VIP. Advisory-
-- locked admission (store.LockAdmission) is the real guard; this partial unique index is the
-- schema backstop, the Proxmox sibling of clusters_vsphere_vip_live. A separate index per provider
-- is correct: each provider's shared subnet is a distinct network, so a vSphere VIP and a Proxmox
-- VIP never collide.
CREATE UNIQUE INDEX clusters_proxmox_vip_live ON clusters (api_vip)
    WHERE deleted_at IS NULL AND api_vip <> '' AND provider = 'proxmox';
