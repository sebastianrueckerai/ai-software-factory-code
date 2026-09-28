-- when the work was asked for (the issue's created_at, or the API call),
-- and the gate the task is in since when: the board reads both
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS gate text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS gate_since timestamptz;

-- a commissioned server's live moment, and whether its commissioning is
-- on the task's timeline (the ones live before this migration never will be)
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS live_at timestamptz;
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS timeline_recorded boolean NOT NULL DEFAULT false;
UPDATE cloud_resources SET timeline_recorded = true WHERE state <> 'requested';

CREATE TABLE IF NOT EXISTS alerts (
    id bigserial PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('gate_bound', 'supervisor_trip', 'escalation')),
    source text NOT NULL,
    task_id text,
    raised_at timestamptz NOT NULL DEFAULT now(),
    bound_s integer,
    resolved_at timestamptz,
    verdict text CHECK (verdict IN ('real', 'noise')),
    resolved_by text,
    detail jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS alerts_task_kind_idx ON alerts (task_id, kind, id);
