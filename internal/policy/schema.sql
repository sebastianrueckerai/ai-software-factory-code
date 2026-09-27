SELECT pg_advisory_xact_lock(hashtext('factory-policy-schema'));

CREATE TABLE IF NOT EXISTS policy_versions (
    id bigserial PRIMARY KEY,
    sha256 text NOT NULL UNIQUE,
    document jsonb NOT NULL,
    loaded_at timestamptz NOT NULL DEFAULT now()
);

-- append-only: the latest row is the version in force
CREATE TABLE IF NOT EXISTS policy_activations (
    id bigserial PRIMARY KEY,
    version_id bigint NOT NULL REFERENCES policy_versions (id),
    activated_at timestamptz NOT NULL DEFAULT now()
);

DO $$ BEGIN
    IF to_regclass('policy_current') IS NOT NULL THEN
        INSERT INTO policy_activations (version_id)
            SELECT version_id FROM policy_current WHERE NOT EXISTS (SELECT 1 FROM policy_activations);
        DROP TABLE policy_current;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS ledger (
    id bigserial PRIMARY KEY,
    task_id text NOT NULL,
    day date NOT NULL DEFAULT (now() AT TIME ZONE 'UTC')::date,
    kind text NOT NULL CHECK (kind IN ('model', 'tool', 'resource', 'settlement')),
    ref text NOT NULL DEFAULT '',
    eur numeric NOT NULL CONSTRAINT ledger_eur_check CHECK (eur >= 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ledger_eur_check') THEN
        ALTER TABLE ledger ADD CONSTRAINT ledger_eur_check CHECK (eur >= 0);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS ledger_day ON ledger (day);
CREATE INDEX IF NOT EXISTS ledger_task ON ledger (task_id);

-- Holds are costs a call or resource may still incur: they count against
-- the task and day headroom until released into the ledger.
CREATE TABLE IF NOT EXISTS holds (
    id bigserial PRIMARY KEY,
    task_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('model', 'tool', 'resource')),
    ref text NOT NULL DEFAULT '',
    eur numeric NOT NULL CHECK (eur >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS holds_task ON holds (task_id);

CREATE OR REPLACE FUNCTION policy_refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME;
END $$;

CREATE OR REPLACE TRIGGER policy_versions_immutable BEFORE UPDATE OR DELETE ON policy_versions
    FOR EACH ROW EXECUTE FUNCTION policy_refuse_change();
CREATE OR REPLACE TRIGGER policy_activations_immutable BEFORE UPDATE OR DELETE ON policy_activations
    FOR EACH ROW EXECUTE FUNCTION policy_refuse_change();
CREATE OR REPLACE TRIGGER ledger_immutable BEFORE UPDATE OR DELETE ON ledger
    FOR EACH ROW EXECUTE FUNCTION policy_refuse_change();
