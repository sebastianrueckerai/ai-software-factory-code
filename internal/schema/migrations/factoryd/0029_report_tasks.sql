-- a report task (ruling 185) reads the issue of its parent task and posts
-- its last message there: the screening of a task's issue, the presort of
-- a suggested improvement
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS parent_task text REFERENCES tasks (id);
CREATE UNIQUE INDEX IF NOT EXISTS tasks_parent_source_key ON tasks (parent_task, source) WHERE parent_task IS NOT NULL;
-- the screening stage: a suspect verdict (ruling 182), one per task: a
-- retried screening finds its row. config_rejected is from Task 72 (0028)
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_kind_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_kind_check CHECK (kind IN ('gate_bound', 'supervisor_trip', 'escalation', 'info_request', 'alert', 'config_rejected', 'screening'));
CREATE UNIQUE INDEX IF NOT EXISTS alerts_screening_task_key ON alerts (task_id) WHERE kind = 'screening';
