-- a rolls row per image a commit changed (ruling 178), the rows before are
-- factoryd's own roll, measured at its boot
ALTER TABLE rolls ADD COLUMN IF NOT EXISTS image text NOT NULL DEFAULT 'factoryd';
ALTER TABLE rolls ADD COLUMN IF NOT EXISTS deployment text NOT NULL DEFAULT 'factoryd';
ALTER TABLE rolls ALTER COLUMN image DROP DEFAULT;
ALTER TABLE rolls ALTER COLUMN deployment DROP DEFAULT;
ALTER TABLE rolls DROP CONSTRAINT rolls_pkey;
ALTER TABLE rolls ADD PRIMARY KEY (sha, image);

-- one row per main commit whose green push run factoryd has judged: the
-- roll task it opened or was folded into, or skipped when it changed no
-- image. changes maps each roll target to the digest main CI published,
-- head is the roll branch commit RollCommit wrote with it
CREATE TABLE IF NOT EXISTS roll_requests (
    sha        text PRIMARY KEY,
    repository text NOT NULL,
    run_id     bigint NOT NULL,
    state      text NOT NULL CHECK (state IN ('opened', 'merged', 'skipped', 'failed')),
    task_id    text,
    changes    jsonb NOT NULL DEFAULT '{}',
    summary    text NOT NULL DEFAULT '',
    committed  boolean NOT NULL DEFAULT false,
    head       text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'skipped') = (task_id IS NULL))
);
CREATE INDEX IF NOT EXISTS roll_requests_task_idx ON roll_requests (task_id);
