-- A pause that names a plan: the group revert of its merged cards holds
-- the project's queue. reverts are the merge shas the revert undoes; both
-- stay empty on a pause for a red main.
ALTER TABLE queue_pauses ADD COLUMN plan text NOT NULL DEFAULT '', ADD COLUMN reverts jsonb NOT NULL DEFAULT '[]';
