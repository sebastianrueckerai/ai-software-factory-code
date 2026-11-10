-- A failed roll request settles with its time, and a commit whose roll the
-- roller tries again keeps its request: retries counts the failed attempts
-- before the current one.
ALTER TABLE roll_requests ADD COLUMN IF NOT EXISTS settled_at timestamptz;
ALTER TABLE roll_requests ADD COLUMN IF NOT EXISTS retries int NOT NULL DEFAULT 0;
UPDATE roll_requests SET settled_at = created_at WHERE state = 'failed' AND settled_at IS NULL;
