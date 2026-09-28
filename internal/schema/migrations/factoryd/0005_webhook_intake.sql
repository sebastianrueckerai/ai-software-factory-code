ALTER TABLE tasks ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'agent';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS issue_number bigint NOT NULL DEFAULT 0;

CREATE UNIQUE INDEX IF NOT EXISTS tasks_repository_issue_number_key
    ON tasks (repository, issue_number) WHERE issue_number <> 0;

CREATE TABLE IF NOT EXISTS intake_events (
    id bigserial PRIMARY KEY,
    task_id text NOT NULL REFERENCES tasks(id),
    kind text NOT NULL,
    author text NOT NULL,
    body text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS intake_events_task_id_idx ON intake_events (task_id);
