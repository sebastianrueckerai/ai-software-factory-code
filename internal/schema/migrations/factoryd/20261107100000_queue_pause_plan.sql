-- A pause that names a plan: the group revert of its merged cards holds
-- the project's queue. reverts are the ids of the cards whose merges the
-- revert undoes; both stay empty on a pause for a red main. An image
-- before this one reads a plan's pause as main's red.
-- A rollback to an image before this one first needs:
--   DELETE FROM queue_pauses WHERE plan <> '';
ALTER TABLE queue_pauses ADD COLUMN plan text NOT NULL DEFAULT '', ADD COLUMN reverts jsonb NOT NULL DEFAULT '[]';
