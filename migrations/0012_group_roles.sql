-- 0012_group_roles.sql - coarse read/write RBAC within a group. A user's group_role governs what
-- they may do to their GROUP-MATES' clusters: 'read' (the default) can only view them, 'write' can
-- also manage them (scale, upgrade, delete, kubeconfig, shell) - the same as the owner. It never
-- restricts a user's own clusters, and is inert for ungrouped users and admins. Enforced in the app
-- layer (see authorizeClusterWrite in internal/app); a plain TEXT column like the other tenancy
-- fields. Existing users default to 'read', the least-privileged role.

ALTER TABLE users ADD COLUMN group_role TEXT NOT NULL DEFAULT 'read';
