-- 0008_health.sql - the latest health snapshot per cluster.
-- Live telemetry, not desired state: the reconciler's health ticker evaluates a set of dedicated
-- checks (API server, nodes Ready, system workloads, scheduling capacity, etcd quorum, add-on
-- availability) against each Ready cluster and upserts one row per cluster here; the API serves it
-- read-through to the portal. Like cluster_metrics we keep only the newest reading (no history -
-- a production system would stream conditions to a monitoring stack), so the row is replaced on
-- every evaluation. `status` is the rolled-up worst-of; the per-check and per-node detail live in
-- JSONB arrays. Cascade-deleted with the cluster.
CREATE TABLE cluster_health (
    cluster_id   TEXT PRIMARY KEY REFERENCES clusters(id) ON DELETE CASCADE,
    collected_at TIMESTAMPTZ NOT NULL,
    status       TEXT NOT NULL,                -- rolled-up domain.HealthStatus
    checks       JSONB NOT NULL DEFAULT '[]',  -- [] of domain.HealthCheck
    nodes        JSONB NOT NULL DEFAULT '[]'   -- [] of domain.NodeHealth
);
