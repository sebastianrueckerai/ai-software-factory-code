-- extra_lines: a card's line bound is its size's and its proofs', and no
-- human raises it, so nothing is left to store.
-- A rollback to an image before this one first needs:
--   ALTER TABLE tasks ADD COLUMN extra_lines integer NOT NULL DEFAULT 0;
ALTER TABLE tasks DROP COLUMN IF EXISTS extra_lines;
