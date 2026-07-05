-- 0032_cluster_repair.sql - observed state for automatic cluster and node repair: what the platform
-- believes is wrong with each of a cluster's nodes, since when, and what it has already tried.
--
-- WHY THIS TABLE EXISTS AT ALL. The platform already DETECTS every fault that matters
-- (internal/health: NotReady nodes, crashlooping system workloads, node pressure, unschedulable
-- capacity) and deliberately does nothing about it - health is decoupled from the state machine and
-- never changes a cluster's phase. Acting on it needs one thing health cannot provide: DURATION.
-- health_snapshots keeps only the newest reading per cluster, so "this node is NotReady" is
-- readable and "this node has been NotReady for twenty minutes" is not - and every safe repair
-- decision is the second sentence, never the first. That is what unhealthy_since is for.
--
-- One JSONB column rather than a node_repair child table: unlike node_pools and node_disks this is
-- OBSERVED state, it is only ever read and written whole by the reconciler, it dies with the
-- cluster, and it is keyed on VM name (like static_ips) rather than on a node id - node rows are
-- re-created whenever their VM is. Nothing queries inside it.
--
-- See internal/domain.RepairPolicy / ClusterRepair and internal/reconcile (PhaseRepairing).
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS repair JSONB NOT NULL DEFAULT '{}'::jsonb;

-- The due-scan (ClustersDueRepair) predicate, same shape as etcd_observed_at in 0030. Held as its
-- own scalar column rather than read out of the JSONB so the index is a plain btree on a timestamp.
--
-- NULL means "never observed": rows predating this feature qualify immediately, so the fleet's
-- repair state backfills itself on the first pass.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS repair_observed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS clusters_repair_observed_at
  ON clusters (repair_observed_at)
  WHERE phase = 'Ready';
