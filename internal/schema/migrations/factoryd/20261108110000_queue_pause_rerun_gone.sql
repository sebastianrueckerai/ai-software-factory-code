-- queue_pauses.rerun_at: factoryd re-runs a red main's failed jobs, and
-- each job's attempt tells its re-run (mainfix.go), so no pause stores one.
-- A rollback to an image before this one first needs:
--   ALTER TABLE queue_pauses ADD COLUMN rerun_at timestamptz;
ALTER TABLE queue_pauses DROP COLUMN IF EXISTS rerun_at;
