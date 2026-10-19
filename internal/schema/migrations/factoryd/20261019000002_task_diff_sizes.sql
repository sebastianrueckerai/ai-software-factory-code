-- The task's diff size (the hard data of a project over time, roadmap
-- 198), written by the revert pass, which holds each diff. NULL until the
-- pass first saw the pull request; 0 is an empty diff.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS files integer;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS insertions integer;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS deletions integer;
