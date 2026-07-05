-- 0006_metrics.sql - the latest resource-usage snapshot per cluster.
-- Live telemetry, not desired state: the reconciler's metrics ticker samples the cluster's
-- in-cluster metrics API (metrics-server) and upserts one row per cluster here; the API serves it
-- read-through to the portal. We keep only the newest reading (no time-series - a demo would
-- stream that to Prometheus/VictoriaMetrics), so the row is replaced on every collection.
-- The per-node samples live in a JSONB array. Cascade-deleted with the cluster.
CREATE TABLE cluster_metrics (
    cluster_id   TEXT PRIMARY KEY REFERENCES clusters(id) ON DELETE CASCADE,
    collected_at TIMESTAMPTZ NOT NULL,
    nodes        JSONB NOT NULL DEFAULT '[]'   -- [] of domain.NodeMetrics
);
