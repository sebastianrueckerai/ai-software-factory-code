-- extra_checkin: the check-in's call cap is one conversation's, not the
-- card's, so a human's answer at the cap widens nothing to store.
-- A rollback to an image before this one first needs:
--   ALTER TABLE tasks ADD COLUMN extra_checkin integer NOT NULL DEFAULT 0;
ALTER TABLE tasks DROP COLUMN IF EXISTS extra_checkin;
