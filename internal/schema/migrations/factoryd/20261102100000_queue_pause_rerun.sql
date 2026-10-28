-- factoryd's one re-run of a red main whose failed jobs all lost their
-- runner; NULL when the pause has none
ALTER TABLE queue_pauses ADD COLUMN IF NOT EXISTS rerun_at timestamptz
