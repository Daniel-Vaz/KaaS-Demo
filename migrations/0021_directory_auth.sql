-- 0021_directory_auth.sql - Active Directory / LDAP accounts alongside the local ones.
--
-- Two provenance columns, both defaulting to 'local' so every existing row keeps its exact current
-- meaning and this migration is a no-op for a local-only deployment.
--
-- users.auth_source: where this account's credential lives.
--   'local' - a bcrypt password_hash in this table (self-registered, or the seeded break-glass admin)
--   'ldap'  - the directory. password_hash is '' and is never consulted.
-- password_hash deliberately stays NOT NULL: an LDAP user stores '', and bcrypt on an empty hash
-- always errors, so an empty hash can never become a login bypass. A nullable column would only
-- invite a NULL-handling mistake on the one path where a mistake is a free login.
ALTER TABLE users ADD COLUMN auth_source TEXT NOT NULL DEFAULT 'local';

-- Directory-supplied profile, purely cosmetic: sAMAccountNames like 'dvaz' make the admin's user
-- table unreadable without them. Empty for local accounts, which have no directory to ask.
ALTER TABLE users ADD COLUMN email        TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';

-- groups.source: who owns this group's roster.
--   'local' - admins manage it in the portal, as today.
--   'ldap'  - a mapping rule in ldap.yaml owns it. The portal shows it read-only; membership is
--             recomputed from the directory on every login of a matching user.
ALTER TABLE groups ADD COLUMN source TEXT NOT NULL DEFAULT 'local';

-- groups.source_key: WHICH mapping rule owns it - the rule's `key`, not its display name.
--
-- Keying on the key rather than the name is what makes `group: "Platform"` → `group: "Platform Eng"`
-- a rename of one group instead of the silent creation of a second one. It is also the schema-level
-- backstop for the boot-time seeding race: every api and worker replica seeds at once, and the
-- partial unique index below is what makes the losers conflict instead of double-creating.
ALTER TABLE groups ADD COLUMN source_key TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX groups_source_key ON groups (source, source_key) WHERE source <> 'local';

-- Note what is NOT here: groups.name keeps its global UNIQUE from 0010_groups.sql. A mapping whose
-- display name collides with an existing LOCAL group is a boot error rather than a silent adoption
-- (see internal/app.ensureDirectoryGroupsLocked) - a config file must not be able to take ownership
-- of a group an admin created by hand.
