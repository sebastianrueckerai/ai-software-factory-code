-- the stall ladder on the head row (pr 0): rerun_at is the one re-run its
-- stalled commit gets, dismissed_at a member's dismissal of the stall;
-- both belong to the commit and clear when a new commit takes the row
ALTER TABLE chain_runs ADD COLUMN IF NOT EXISTS rerun_at timestamptz;
ALTER TABLE chain_runs ADD COLUMN IF NOT EXISTS dismissed_at timestamptz;
