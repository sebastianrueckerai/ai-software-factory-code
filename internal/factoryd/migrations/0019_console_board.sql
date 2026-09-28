-- the console board's finished tasks and its escalation reasons (task 54)
CREATE INDEX IF NOT EXISTS tasks_finished_at_idx ON tasks (finished_at) WHERE status IN ('done', 'failed');
CREATE INDEX IF NOT EXISTS intake_events_task_kind_idx ON intake_events (task_id, kind, id);
