CREATE TABLE IF NOT EXISTS policy_versions (
    id bigserial PRIMARY KEY,
    sha256 text NOT NULL UNIQUE,
    document jsonb NOT NULL,
    loaded_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS policy_current (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    version_id bigint NOT NULL REFERENCES policy_versions (id)
);

CREATE TABLE IF NOT EXISTS ledger (
    id bigserial PRIMARY KEY,
    task_id text NOT NULL,
    day date NOT NULL DEFAULT (now() AT TIME ZONE 'UTC')::date,
    kind text NOT NULL CHECK (kind IN ('model', 'tool', 'resource', 'settlement')),
    ref text NOT NULL DEFAULT '',
    eur numeric NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ledger_day ON ledger (day);
CREATE INDEX IF NOT EXISTS ledger_task ON ledger (task_id);
