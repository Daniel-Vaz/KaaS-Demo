-- 0011_operation_actor.sql - attribute each recorded operation to the user who triggered it.
-- Denormalized (actor_username copied at write time, not joined) so the audit trail stays
-- meaningful even after the actor is later deleted or renamed.

ALTER TABLE operations ADD COLUMN actor_id TEXT NOT NULL DEFAULT '';
ALTER TABLE operations ADD COLUMN actor_username TEXT NOT NULL DEFAULT '';
