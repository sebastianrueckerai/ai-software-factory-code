CREATE TABLE IF NOT EXISTS tasks (
    id text PRIMARY KEY,
    priority integer NOT NULL DEFAULT 0,
    required_capabilities jsonb NOT NULL DEFAULT '[]',
    risk_class text NOT NULL DEFAULT 'standard',
    budget_eur numeric NOT NULL,
    spent_eur numeric NOT NULL DEFAULT 0,
    status text NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id text PRIMARY KEY,
    task_id text NOT NULL,
    parent_session_id text NOT NULL DEFAULT '',
    findings jsonb NOT NULL DEFAULT '[]',
    restart_state jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS priority integer NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS required_capabilities jsonb NOT NULL DEFAULT '[]';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS risk_class text NOT NULL DEFAULT 'standard';
