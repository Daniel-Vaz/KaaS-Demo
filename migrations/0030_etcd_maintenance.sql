-- 0030_etcd_maintenance.sql - observed state for automatic etcd maintenance: the size and
-- fragmentation of the cluster's etcd backend store, the alarms it has armed, and when the platform
-- last defragmented it.
--
-- etcd compaction (which the apiserver runs every 5m) frees keyspace LOGICALLY but never shrinks the
-- bbolt file; only defragmentation does. Left alone, the file grows to its high-water mark and stays
-- there, and on reaching --quota-backend-bytes etcd arms NOSPACE and the WHOLE cluster goes
-- read-only. The reconciler re-reads these columns on a cadence (KAAS_ETCD_OBSERVE_INTERVAL) and
-- promotes into PhaseDefragmentingEtcd when the fragmentation thresholds are met inside the
-- maintenance window - or immediately, window and hysteresis ignored, when NOSPACE is already armed.
--
-- NULL etcd_observed_at means "never observed": fresh clusters stamp it on their first Ready tick,
-- and rows predating this feature (default NULL) qualify for observation immediately, exactly like
-- cert_not_after in 0025.
--
-- See internal/domain.EtcdStatus / EtcdDefragPolicy and internal/reconcile (PhaseDefragmentingEtcd).
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS etcd_db_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS etcd_db_in_use_bytes BIGINT NOT NULL DEFAULT 0;
-- 0 = the member carries no --quota-backend-bytes flag, i.e. etcd's own 2GiB default.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS etcd_quota_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS etcd_alarms TEXT[] NOT NULL DEFAULT '{}';
-- How many members answered the status read. Fewer than the cluster's control planes means one is
-- unreachable, which is a hard block on defragmenting (defrag blocks the member it runs on).
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS etcd_members INTEGER NOT NULL DEFAULT 0;
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS etcd_observed_at TIMESTAMPTZ;
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS etcd_defragged_at TIMESTAMPTZ;

-- The due-scan (ClustersDueEtcdMaintenance) is a per-tick query over live clusters keyed on staleness
-- of the observation, so index the predicate rather than making every tick a seq scan as the fleet
-- grows. Partial on the phase the scan restricts to, like the other live-cluster indexes.
CREATE INDEX IF NOT EXISTS clusters_etcd_observed_at
  ON clusters (etcd_observed_at)
  WHERE phase = 'Ready';
