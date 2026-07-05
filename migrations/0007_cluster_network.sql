-- 0007_cluster_network.sql - dedicated, isolated per-cluster libvirt network.
-- Each cluster now provisions its own NAT bridge (infra/libvirt); network_cidr is the CIDR its VMs
-- and the HA API VIP draw addresses from, chosen at create time (auto-allocated from the platform
-- supernet, or user-supplied). Distinct from the Kubernetes-internal pod_cidr/svc_cidr.
-- See internal/netpool and docs/networking.md.
ALTER TABLE clusters ADD COLUMN network_cidr TEXT NOT NULL DEFAULT '';
