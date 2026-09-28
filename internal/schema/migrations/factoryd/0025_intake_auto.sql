-- an agent's request for the issue author's information (ruling 150)
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_kind_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_kind_check CHECK (kind IN ('gate_bound', 'supervisor_trip', 'escalation', 'info_request'));

-- the issue's author and whether they could write the repository at
-- intake (ruling 152), and the agent's last word on its pushed work: its
-- recommendation, and a not_reproduced line when it fixed a fault it did
-- not reproduce exactly (ruling 149)
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS author text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS author_writer boolean NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS agent_summary text NOT NULL DEFAULT '';

-- closed: an issue closed as not reproducible (ruling 150) ends its task
DROP INDEX IF EXISTS tasks_finished_at_idx;
CREATE INDEX IF NOT EXISTS tasks_finished_at_idx ON tasks (finished_at) WHERE status IN ('done', 'failed', 'closed');
