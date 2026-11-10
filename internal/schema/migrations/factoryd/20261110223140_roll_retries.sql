-- A failed roll request settles with its time, and a commit whose roll the
-- roller tries again keeps its request: failed_tasks are the roll tasks of
-- its earlier failed attempts, oldest first.
ALTER TABLE roll_requests ADD COLUMN IF NOT EXISTS settled_at timestamptz;
ALTER TABLE roll_requests ADD COLUMN IF NOT EXISTS failed_tasks text[] NOT NULL DEFAULT '{}';
UPDATE roll_requests SET settled_at = created_at WHERE state = 'failed' AND settled_at IS NULL;
