-- factoryd reads the month cap's refusals: a task's since its session
-- began, and whether the month has any
CREATE INDEX IF NOT EXISTS audit_month_cap_idx ON audit (task_id, ts) WHERE decision = 'refused_month_cap';
