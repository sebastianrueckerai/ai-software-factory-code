-- the lanes collapse (roadmap 225): whoever files an agent card sets its
-- track, plan or direct, and the schema refuses an agent card without one;
-- the lane goes, and a backlog card, which waited for a decision, ends
-- closed with the write-up it waited on: a queue holds only work waiting
-- for capacity
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS track text CHECK (track IN ('direct', 'plan'));
UPDATE tasks SET track = CASE lane WHEN 'plan' THEN 'plan' ELSE 'direct' END WHERE kind = 'agent' AND track IS NULL;
ALTER TABLE tasks ADD CONSTRAINT tasks_agent_track CHECK (kind <> 'agent' OR track IS NOT NULL);
UPDATE tasks SET status = 'closed', gate = '', gate_since = NULL, finished_at = COALESCE(finished_at, now()) WHERE status = 'backlog'
   OR (source = 'presort' AND status NOT IN ('done', 'failed', 'closed'));
ALTER TABLE tasks DROP COLUMN IF EXISTS lane
