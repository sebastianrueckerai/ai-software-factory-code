-- a signal card's pinned reproduction base (roadmap 194): the commit
-- that was live when its alert fired, recorded on the task row at filing
-- from the deployed build's roll. The reproduction gate runs the base
-- against that tree even though main has moved on, and SetTaskSHA's
-- session-start stamping of sha never touches it. NULL for every other
-- card, whose reproduction base stays sha.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS repro_base text;
