-- 0005_operations.sql - the per-cluster action/audit history.
-- Higher-level than the `events` table (which is the per-tick reconciler timeline): one row
-- records one user intent (create / scale / add-ons / upgrade) and its from→to detail. The app
-- inserts a row (in_progress) when it writes desired state; the reconciler flips it to completed
-- once observed_generation catches up to the recording generation. Cascade-deleted with the
-- cluster, matching nodes/addons/events.
CREATE TABLE operations (
    id          TEXT PRIMARY KEY,
    cluster_id  TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,                 -- create | scale | addons | upgrade
    summary     TEXT NOT NULL,                 -- human-readable one-liner
    detail      TEXT NOT NULL DEFAULT '',      -- optional extra context (e.g. per-component upgrade diff)
    generation  BIGINT NOT NULL,               -- the cluster generation this operation produced
    status      TEXT NOT NULL,                 -- in_progress | completed
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX operations_cluster_started ON operations (cluster_id, started_at DESC);
-- The reconciler completes still-open operations up to a generation once a cluster converges.
CREATE INDEX operations_open ON operations (cluster_id, generation) WHERE status = 'in_progress';
