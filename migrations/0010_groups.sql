-- 0010_groups.sql - admin-managed teams. Members of the same group get full access to each
-- other's clusters (see authorizeCluster in internal/app). One group per user (nullable via empty
-- string, matching the owner_id convention); no FK, enforced in the app layer.

CREATE TABLE groups (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE users ADD COLUMN group_id TEXT NOT NULL DEFAULT '';
CREATE INDEX users_group ON users (group_id);
