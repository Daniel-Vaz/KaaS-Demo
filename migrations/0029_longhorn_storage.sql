-- 0029_longhorn_storage.sql - default cluster storage: every worker is born with an extra disk
-- mounted at /var/lib/longhorn, which the bundled longhorn add-on registers as that node's Longhorn
-- disk and uses to back the cluster's default StorageClass. storage_disk_gb is desired state chosen
-- once at creation and immutable afterwards (a NodeDisk's size cannot change in place - capacity is
-- grown by attaching another disk, which is Longhorn's own model); the disks themselves are ordinary
-- rows in node_disks, so this feature adds no storage concept of its own.
--
-- storage_wired is the observed-state marker for the registration step (reconcileStorageWiring / the
-- longhorn_disks ansible role), and unlike gateway_wired/dns_wired it is a TEXT FINGERPRINT of the
-- disk set rather than a boolean. That is load-bearing: the gateway's CRs are decided at admission
-- and never move, but a user attaches and removes storage disks on a running cluster and each change
-- has to reach the node.longhorn.io CRs - a boolean would latch on the first disk and silently
-- strand every later one.
--
-- Pre-existing rows default to 0 / '': they own no platform storage disk, their longhorn add-on (if
-- any) keeps using each node's root disk exactly as before, and the wiring never fires.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS storage_disk_gb INT  NOT NULL DEFAULT 0;
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS storage_wired   TEXT NOT NULL DEFAULT '';
