-- 0031_etcd_snapshots.sql - periodic control-plane backups: an `etcdctl snapshot save` of the
-- cluster's keyspace together with its PKI and kubelet state, sealed and held here.
--
-- This is what makes a dead SOLE control plane recoverable. The existing rolling-replacement path
-- copies a control plane's state off it just before destroying it (backup-controlplane.yml), which
-- works only while the node is alive - exactly the assumption that fails when the node is the thing
-- that broke. A snapshot taken on a cadence, off the box, is the only state a rebuild can start from.
--
-- WHY THE PAYLOAD LIVES IN POSTGRES, sealed, rather than on a worker's disk: worker replicas are
-- stateless and interchangeable (deploy/compose.scale.yaml runs N of them), so the replica that has
-- to restore is not the one that took the snapshot. Postgres is the only durable store every replica
-- shares. And it is SEALED with secrets.Box because an etcd snapshot is the entire cluster's Secrets
-- in plaintext plus the CA private key - strictly more sensitive than the admin kubeconfig, which is
-- only a credential TO the cluster rather than a copy OF it. See internal/domain.EtcdSnapshot.
--
-- Production would put the payload in object storage under a KMS-backed key with its own retention
-- and audit policy; this table is the demo-scale stand-in, capped by domain.MaxEtcdSnapshotBytes and
-- pruned to KAAS_ETCD_SNAPSHOT_RETAIN per cluster.
CREATE TABLE IF NOT EXISTS etcd_snapshots (
  id          TEXT PRIMARY KEY,
  cluster_id  TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
  taken_at    TIMESTAMPTZ NOT NULL,
  -- The etcd store revision the snapshot captured, from `etcdutl snapshot status`. This is what
  -- makes "how much would a restore roll back" an answerable question.
  revision    BIGINT NOT NULL DEFAULT 0,
  -- The snapshot file's own integrity hash, from the same verification step. A file that could not
  -- be hashed is not a backup and is never stored, so this is always the hash of a verified file.
  hash        BIGINT NOT NULL DEFAULT 0,
  size_bytes  BIGINT NOT NULL DEFAULT 0,
  k8s_version TEXT NOT NULL DEFAULT '',
  node_name   TEXT NOT NULL DEFAULT '',
  -- The sealed archive: etcd snapshot + /etc/kubernetes + /var/lib/kubelet. Never served through
  -- any API surface, never written unsealed outside the worker's own artifacts dir.
  payload     BYTEA NOT NULL
);

-- Retention and restore both walk one cluster's snapshots newest-first.
CREATE INDEX IF NOT EXISTS etcd_snapshots_cluster_taken
  ON etcd_snapshots (cluster_id, taken_at DESC);

-- The due-scan (ClustersDueEtcdSnapshot) is a per-tick query over live clusters keyed on staleness,
-- so it gets the same treatment as etcd_observed_at in 0030: a scalar on the cluster row with a
-- partial index, rather than an aggregate over a table whose rows are megabytes each.
--
-- NULL means "never snapshotted", which every row predating this feature is - so they all qualify
-- immediately and the fleet backfills itself, exactly like cert_not_after in 0025.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS etcd_snapshot_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS clusters_etcd_snapshot_at
  ON clusters (etcd_snapshot_at)
  WHERE phase = 'Ready';
