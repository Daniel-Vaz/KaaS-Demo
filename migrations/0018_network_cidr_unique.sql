-- 0018_network_cidr_unique.sql - a cluster's node network is exclusive to it.
--
-- internal/netpool allocates the CIDR by reading every live cluster and picking a free block. That
-- read-then-write is now serialized across API replicas by an advisory lock (store.LockAdmission),
-- but the invariant it protects belongs in the schema too: this index is the backstop that makes a
-- double-allocation impossible rather than merely unlikely - a second cluster on the same subnet
-- would silently break routing for both.
--
-- Partial, like clusters_owner_name_live (0009): a deleted cluster releases its network back to the
-- pool, and pre-0007 rows carry the '' default, which is not an allocation.
CREATE UNIQUE INDEX clusters_network_cidr_live ON clusters (network_cidr)
    WHERE deleted_at IS NULL AND network_cidr <> '';
