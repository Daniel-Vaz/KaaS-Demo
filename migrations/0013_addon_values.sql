-- 0013_addon_values.sql - per-cluster Helm values override for an add-on. A user may customize an
-- add-on's Helm values (via the in-browser editor, seeded with the chart's values.yaml merged with
-- the platform's curated catalog defaults). The full edited YAML document is stored here; empty
-- means "use the catalog's curated --set defaults" (the historical behaviour). When non-empty, the
-- reconciler installs the add-on with `helm ... -f <override>` and skips the catalog --set flags.
-- See internal/domain.Addon.ValuesOverride and internal/addons.

ALTER TABLE cluster_addons ADD COLUMN values_override TEXT NOT NULL DEFAULT '';
