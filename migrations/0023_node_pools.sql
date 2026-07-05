-- 0023_node_pools.sql - worker nodes move from one flat count into named node pools.
--
-- Before: a cluster had `size` (every node) + `desired_workers` (a single, homogeneous worker set).
-- After:  `size` sizes the CONTROL PLANE only, and each pool sizes its own workers. The total worker
-- count is derived from the pools (domain.Cluster.WorkerCount), so `desired_workers` is dropped
-- rather than kept - the pools are the single writer of worker topology, and a cached total could
-- only ever drift from them.
--
-- Mirrors internal/domain.NodePool. Pools follow cluster_addons exactly: a child table keyed on
-- (cluster_id, name), rewritten wholesale with the cluster, ordered by `position` (see
-- 0016_addon_position.sql for the same reasoning - the list is user-ordered desired state).

CREATE TABLE node_pools (
    cluster_id      TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    size            TEXT NOT NULL,          -- t-shirt size of THIS pool's workers
    desired_workers INT  NOT NULL DEFAULT 0,
    position        INT  NOT NULL DEFAULT 0,
    PRIMARY KEY (cluster_id, name)
);

-- Which pool owns each node. Empty for control planes, which belong to no pool.
ALTER TABLE nodes ADD COLUMN pool TEXT NOT NULL DEFAULT '';

-- Backfill every existing cluster with the "default" pool it would have been created with. The size
-- is faithful: before this migration a cluster's workers genuinely did run at clusters.size.
INSERT INTO node_pools (cluster_id, name, size, desired_workers, position)
SELECT id, 'default', size, desired_workers, 0 FROM clusters;

UPDATE nodes SET pool = 'default' WHERE role = 'worker';

ALTER TABLE clusters DROP COLUMN desired_workers;

-- NOTE - a one-time worker roll on existing clusters. Node VM names now encode the owning pool
-- ("<cluster>-default-0"), where they used to be "<cluster>-w-0". The rows backfilled above keep
-- their old names, so on the first tick after this migration the reconciler sees them missing from
-- the desired set: each existing cluster goes Ready -> Updating and drains, destroys and recreates
-- its workers under the new names. That is correct and convergent (control planes are untouched, and
-- the drain is the same graceful path a scale-down takes), but it IS disruptive - worker workloads
-- are rescheduled once. Disposable demo clusters are cheaper torn down first
-- (deploy/teardown-clusters.sh) than rolled.
