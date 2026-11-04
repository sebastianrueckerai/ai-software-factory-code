-- factoryd reads a running card's newest model call, the newest audit
-- row of the task, for every running card's digest
CREATE INDEX IF NOT EXISTS audit_task_ts_idx ON audit (task_id, ts);
