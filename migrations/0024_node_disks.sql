-- 0024_node_disks.sql - per-pool root disk sizing, per-node extra disks, and disk as a quota
-- dimension.
--
-- Three related changes, in one migration because the third is what makes the first two safe:
--
--   1. node_pools.disk_gb - a pool may size its workers' ROOT disk above the t-shirt default
--      (0 = use the default; see domain.NodePool.RootDiskGB). Control planes are unaffected: they
--      are not in a pool.
--   2. node_disks - EXTRA block devices attached to individual worker nodes, formatted and mounted
--      with LVM by Ansible (see domain.NodeDisk, ansible/roles/node_disks).
--   3. users.quotas.*.disk_gb - storage becomes a metered dimension alongside vcpu/mem_mb, because
--      (1) and (2) are precisely the two ways a tenant can now spend a host's disk without bound.

-- 1. The pool's root-disk override. 0 = the size's default, which is what every existing pool used.
ALTER TABLE node_pools ADD COLUMN disk_gb INT NOT NULL DEFAULT 0;

-- 2. Extra disks. Keyed on (cluster_id, vm_name, name) rather than a node id: a node row is
-- OBSERVED state and is re-created whenever its VM is (a rolling OS replacement mints a new id),
-- while the VM NAME is the stable identity the provisioner converges on. So a node rebuilt
-- underneath keeps its disks. Same reasoning as clusters.static_ips.
--
-- No FK to nodes for exactly that reason - the node row may legitimately not exist yet (the disk is
-- desired before the VM is built) or momentarily at all (mid-rebuild). The cluster FK is the real
-- ownership edge, and domain.ValidateNodeDisks is what keeps vm_name pointing at a desired worker.
CREATE TABLE node_disks (
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    vm_name    TEXT NOT NULL,
    name       TEXT NOT NULL,          -- logical name, unique per NODE; names the LVM volume group
    size_gb    INT  NOT NULL,
    mount_path TEXT NOT NULL,
    fs_type    TEXT NOT NULL,          -- ext4 | xfs
    phase      TEXT NOT NULL,          -- pending | attached | removing (see domain.NodeDisk)
    wwn        TEXT NOT NULL DEFAULT '', -- kvm: the platform-minted stable identity
    device_id  TEXT NOT NULL DEFAULT '', -- observed /dev/disk/by-id/ name, reported by the provisioner
    position   INT  NOT NULL DEFAULT 0,
    PRIMARY KEY (cluster_id, vm_name, name)
);

-- The reconciler's per-node convergence reads a node's disks; the portal's node pane reads them per
-- cluster. The PK already covers (cluster_id, vm_name) as a prefix, so no extra index is needed.

-- 3. Disk becomes a quota dimension. Every existing grant predates it and would therefore read as a
-- ZERO disk grant - which, since every node has a root disk, would lock every tenant out of creating
-- anything at all the moment this ships. So backfill each grant with the disk its EXISTING vcpu
-- grant could already have spent: the platform's default node ratio is 40 GB per 2 vCPU (see
-- domain.Sizes), i.e. 20 GB per vCPU. A tenant granted 8 vCPU could already run 4 default nodes =
-- 160 GB, and that is exactly what they get. No tenant gains or loses the ability to build anything
-- they could build before.
--
-- The conserved-pool invariant survives by the same arithmetic: the grants' vcpu already sum to at
-- most the KVM ceiling (16 by default), so the backfilled disk sums to at most 16 × 20 = 320 GB,
-- inside the 500 GB default KAAS_BUDGET_DISK_GB. An operator running a custom ceiling should check
-- the Admin page's allocation summary after upgrading; over-allocation is inert (nothing new is
-- admitted against it) and is corrected by any grant edit, which re-checks the pool.
--
-- Admin rows are included but immaterial - admins hold no stored grant (quota.Allocated skips them).
UPDATE users u
SET quotas = (
    SELECT jsonb_object_agg(
        k,
        CASE WHEN v ? 'disk_gb' THEN v
             ELSE v || jsonb_build_object('disk_gb', COALESCE((v ->> 'vcpu')::int, 0) * 20)
        END)
    FROM jsonb_each(u.quotas) AS e(k, v)
)
WHERE quotas IS NOT NULL AND quotas <> '{}'::jsonb;
