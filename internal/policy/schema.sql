SELECT pg_advisory_xact_lock(hashtext('factory-policy-schema'));

-- one row per commit of the infra repository's main whose factory/ tree
-- loaded: the whole tree as one document (ruling 183)
CREATE TABLE IF NOT EXISTS policy_versions (
    id bigserial PRIMARY KEY,
    commit_sha text NOT NULL UNIQUE,
    document jsonb NOT NULL,
    loaded_at timestamptz NOT NULL DEFAULT now()
);

-- rows from before the tree keep their file's sha256 in commit_sha; their
-- documents are no tree, so no binary takes them for a valid version
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name = 'policy_versions' AND column_name = 'sha256' AND table_schema = current_schema()) THEN
        ALTER TABLE policy_versions RENAME COLUMN sha256 TO commit_sha;
    END IF;
END $$;

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
    kind text NOT NULL CONSTRAINT ledger_kind_check CHECK (kind IN ('model', 'tool', 'resource', 'settlement', 'standing')),
    ref text NOT NULL DEFAULT '',
    -- a final settlement (ruling 196) may lower the spend of a task
    eur numeric NOT NULL CONSTRAINT ledger_eur_check CHECK (eur >= 0 OR kind = 'settlement'),
    created_at timestamptz NOT NULL DEFAULT now()
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ledger_eur_check' AND pg_get_constraintdef(oid) LIKE '%settlement%') THEN
        ALTER TABLE ledger DROP CONSTRAINT IF EXISTS ledger_eur_check;
        ALTER TABLE ledger ADD CONSTRAINT ledger_eur_check CHECK (eur >= 0 OR kind = 'settlement');
    END IF;
END $$;

-- a standing resource's cost is its project's monthly line, no task's
-- spend and no day's headroom (ruling 147)
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ledger_kind_check'
                   AND pg_get_constraintdef(oid) LIKE '%standing%') THEN
        ALTER TABLE ledger DROP CONSTRAINT IF EXISTS ledger_kind_check;
        ALTER TABLE ledger ADD CONSTRAINT ledger_kind_check
            CHECK (kind IN ('model', 'tool', 'resource', 'settlement', 'standing'));
    END IF;
END $$;

-- a model call's usage as the provider reported it and its request
-- body's bytes; 0 on every other row
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_name = 'ledger' AND column_name = 'body_bytes' AND table_schema = current_schema()) THEN
        ALTER TABLE ledger ADD COLUMN prompt_tokens integer NOT NULL DEFAULT 0, ADD COLUMN cached_tokens integer NOT NULL DEFAULT 0,
            ADD COLUMN output_tokens integer NOT NULL DEFAULT 0, ADD COLUMN body_bytes integer NOT NULL DEFAULT 0;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS ledger_day ON ledger (day);
CREATE INDEX IF NOT EXISTS ledger_task ON ledger (task_id);
-- a ref's newest model calls, which measure its cache floor
CREATE INDEX IF NOT EXISTS ledger_model_ref ON ledger (ref, id) WHERE kind = 'model';

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
