-- 0016_addon_position.sql - preserve add-on install order across a store round-trip. The reconciler
-- installs add-ons in c.Addons slice order, and internal/app.resolveAddons deliberately pins that
-- order (bundled platform add-ons first - kube-prometheus-stack ahead of everything so its
-- ServiceMonitor CRD exists before a dependent add-on renders one). The store previously loaded
-- add-ons `ORDER BY name`, which silently re-sorted them alphabetically ("kepler" < "kube-prometheus-
-- stack"), so kepler installed first in real (Postgres) mode and failed on the missing CRD. Persist
-- the intended index and load by it. Existing rows default to 0 and fall back to a name tiebreak,
-- then get their real positions on the next write. See internal/store/postgres.writeChildren/load.
ALTER TABLE cluster_addons ADD COLUMN IF NOT EXISTS position INTEGER NOT NULL DEFAULT 0;
