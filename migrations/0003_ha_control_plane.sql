-- HA control plane (ADR: control-plane topology). control_planes is the number of
-- control-plane VMs (1 = single node, 3 = HA/stacked-etcd). api_vip is the keepalived
-- floating VIP fronting the API servers for HA clusters (empty for single-node). Existing
-- rows default to a single-node control plane, matching prior behaviour.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS control_planes INT  NOT NULL DEFAULT 1;
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS api_vip        TEXT NOT NULL DEFAULT '';
