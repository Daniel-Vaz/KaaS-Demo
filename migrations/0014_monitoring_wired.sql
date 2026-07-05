-- 0014_monitoring_wired.sql - observed-state marker: has this cluster's control plane and CNI been
-- made Prometheus-scrapeable yet? The wiring (kubeadm manifest edits + a kube-proxy ConfigMap patch,
-- and a CNI helm upgrade for its ServiceMonitor) is idempotent but not free, so the reconciler skips
-- it on unrelated update ticks (e.g. an add-on values edit) once wired. Existing rows default to
-- FALSE, so they get re-wired once on their next reconcile - a harmless no-op if already scrapeable.
-- See internal/domain.Cluster.MonitoringWired and internal/reconcile.reconcileMonitoringWiring.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS monitoring_wired BOOLEAN NOT NULL DEFAULT FALSE;
