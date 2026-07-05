-- 0015_group_memberships.sql - multi-group membership. A user may now belong to several groups at
-- once, holding an independent read/write role in each (see internal/domain.GroupMembership). This
-- replaces the single users.group_id/group_role pair (added in 0010/0012) with a join table keyed by
-- (user_id, group_id). The role has the same meaning as before - 'read' (default) may only view that
-- group's members' clusters, 'write' may also manage them - but is now scoped per group. No FKs,
-- matching the app-layer tenancy convention (enforced in internal/app / internal/store).

CREATE TABLE group_memberships (
    user_id  TEXT NOT NULL,
    group_id TEXT NOT NULL,
    role     TEXT NOT NULL DEFAULT 'read',
    PRIMARY KEY (user_id, group_id)
);
CREATE INDEX group_memberships_group ON group_memberships (group_id);

-- Backfill each user's single existing membership, then retire the columns it lived in.
INSERT INTO group_memberships (user_id, group_id, role)
    SELECT id, group_id, group_role FROM users WHERE group_id <> '';

DROP INDEX IF EXISTS users_group;
ALTER TABLE users DROP COLUMN group_id;
ALTER TABLE users DROP COLUMN group_role;
