-- Record the CNI's pinned version as provenance alongside its name (ADR-011/ADR-013).
-- Existing rows default to '' - the config manager then falls back to the CNI role's
-- built-in default version for those clusters.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS cni_version TEXT NOT NULL DEFAULT '';
