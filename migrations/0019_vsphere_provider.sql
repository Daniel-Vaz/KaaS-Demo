-- 0019_vsphere_provider.sql - multi-provider clusters (vSphere alongside KVM).
--
-- provider records which infrastructure a cluster is provisioned on; pre-existing rows are
-- kvm. network_name/ip_mode/net_gateway/net_dns are the vSphere network spec copied from the
-- deployment settings at admission (desired state lives in the DB, not env). static_ips is the
-- per-node IP allocation for vSphere static mode, keyed by vm_name so a re-created node keeps
-- its address (the stable-IP contract rolling OS replacement depends on).
ALTER TABLE clusters ADD COLUMN provider     TEXT  NOT NULL DEFAULT 'kvm';
ALTER TABLE clusters ADD COLUMN network_name TEXT  NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN ip_mode      TEXT  NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN net_gateway  TEXT  NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN net_dns      TEXT  NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN static_ips   JSONB NOT NULL DEFAULT '{}'::jsonb;

-- vSphere clusters share the operator's portgroup subnet, so per-cluster CIDR exclusivity
-- (the 0018 backstop) is a KVM-only invariant now.
DROP INDEX clusters_network_cidr_live;
CREATE UNIQUE INDEX clusters_network_cidr_live ON clusters (network_cidr)
    WHERE deleted_at IS NULL AND network_cidr <> '' AND provider = 'kvm';

-- On that shared subnet the HA API VIP is the per-cluster-exclusive resource instead. Advisory-
-- locked admission (store.LockAdmission) is the real guard; this is the schema backstop. Per-node
-- static IPs live in JSONB where no such index is expressible - a production system would use a
-- dedicated IPAM table with a UNIQUE(ip) constraint.
CREATE UNIQUE INDEX clusters_vsphere_vip_live ON clusters (api_vip)
    WHERE deleted_at IS NULL AND api_vip <> '' AND provider = 'vsphere';
