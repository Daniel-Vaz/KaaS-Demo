-- 0017_custom_catalogs.sql - per-user custom add-on catalogs. A custom catalog is a user-owned,
-- named collection of self-defined Helm-chart add-ons (name, description, repo/chart/version,
-- namespace, values). Ownership + sharing mirror clusters: the owner and admins have full access,
-- and group-mates share access via their per-group read/write role (enforced in internal/app). See
-- internal/domain.CustomCatalog / CustomAddon.
--
-- When a custom add-on is selected onto a cluster its chart definition is COPIED into the
-- cluster_addons row (self-contained, like values_override), so the untenanted reconcile loop never
-- resolves a custom catalog. The extra cluster_addons columns below carry that copy; empty for a
-- built-in catalog add-on.

CREATE TABLE custom_catalogs (
    id         TEXT PRIMARY KEY,
    owner_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_id, name)
);

CREATE TABLE custom_catalog_addons (
    catalog_id  TEXT NOT NULL REFERENCES custom_catalogs(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    repo        TEXT NOT NULL DEFAULT '',   -- classic HTTP chart-repo URL (empty for oci:// chart)
    chart       TEXT NOT NULL,              -- oci:// ref or chart name
    version     TEXT NOT NULL,
    namespace      TEXT NOT NULL DEFAULT '',   -- install namespace (empty = same as name)
    default_values TEXT NOT NULL DEFAULT '',   -- full Helm values YAML (empty = chart defaults); "values" is reserved
    position       INT  NOT NULL DEFAULT 0,    -- authoring order, preserved on reload
    PRIMARY KEY (catalog_id, name)
);

-- The self-contained copy of a custom add-on's chart definition, carried on the cluster so the helm
-- manager installs it without a catalog lookup. All empty for a built-in catalog add-on.
ALTER TABLE cluster_addons ADD COLUMN catalog_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_addons ADD COLUMN chart       TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_addons ADD COLUMN repo        TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_addons ADD COLUMN namespace   TEXT NOT NULL DEFAULT '';
ALTER TABLE cluster_addons ADD COLUMN description TEXT NOT NULL DEFAULT '';
