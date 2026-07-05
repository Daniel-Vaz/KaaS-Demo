-- 0009_users.sql - local-account multi-tenancy.
-- Adds the users table and scopes clusters to an owner. Quota moves from a single global budget
-- to per-user allocations under a conserved-pool invariant enforced in the app (internal/quota):
-- the sum of all users' quota can never exceed the platform total. Ownership is enforced in the
-- app layer (owner-or-admin), so owner_id is a plain TEXT column (IDs are app-generated strings,
-- and the admin is seeded at runtime - see internal/app.ensureAdmin - after this migration runs).

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,          -- bcrypt (see internal/auth)
    is_admin      BOOLEAN NOT NULL DEFAULT false,
    quota_vcpu    INT NOT NULL DEFAULT 0, -- self-registered users start at zero
    quota_mem_mb  INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owner of each cluster. Empty for rows that predate tenancy; the app backfills them to the
-- seeded admin on startup (ensureAdmin).
ALTER TABLE clusters ADD COLUMN owner_id TEXT NOT NULL DEFAULT '';
CREATE INDEX clusters_owner ON clusters (owner_id);

-- Cluster names are now unique per owner (two tenants may each have a "dev"), among live clusters
-- only - a soft-deleted name is reusable, matching the app's admission check. Replaces the old
-- global UNIQUE(name) constraint from 0001_init.sql.
ALTER TABLE clusters DROP CONSTRAINT clusters_name_key;
CREATE UNIQUE INDEX clusters_owner_name_live ON clusters (owner_id, name) WHERE deleted_at IS NULL;
