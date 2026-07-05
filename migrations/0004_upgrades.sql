-- Cluster upgrades (bundle promotion). target_bundle is the bundle the user asked to promote to
-- (desired); the reconciler advances the cluster one supersedes hop at a time until bundle catches
-- up (empty = no upgrade in progress). nodes.image records the golden image each VM was cloned
-- from, so rolling OS replacement can resume one node at a time. Existing rows default to no
-- pending upgrade / unknown image, matching prior behaviour.
ALTER TABLE clusters ADD COLUMN IF NOT EXISTS target_bundle TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes    ADD COLUMN IF NOT EXISTS image         TEXT NOT NULL DEFAULT '';
