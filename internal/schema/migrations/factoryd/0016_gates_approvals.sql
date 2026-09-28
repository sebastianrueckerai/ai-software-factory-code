ALTER TABLE tasks ADD COLUMN IF NOT EXISTS repro_command text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS approval_requests (
    id bigserial PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('merge', 'resource')),
    task_id text NOT NULL,
    project text NOT NULL,
    pr_number bigint,
    spec jsonb NOT NULL DEFAULT '{}',
    reason text NOT NULL DEFAULT '',
    monthly_eur numeric,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'denied')),
    decided_by text NOT NULL DEFAULT '',
    decided_at timestamptz,
    decision_note text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS approval_one_pending_merge ON approval_requests (task_id) WHERE kind = 'merge' AND status = 'pending';
CREATE INDEX IF NOT EXISTS approval_requests_status ON approval_requests (status);
