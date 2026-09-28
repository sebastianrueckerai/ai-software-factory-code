-- agent commissioning (task 53): who a resource is for and what it cost
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS repo text NOT NULL DEFAULT '';
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS session_ref text NOT NULL DEFAULT '';
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS size text NOT NULL DEFAULT '';
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS purpose text NOT NULL DEFAULT '';
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS address text NOT NULL DEFAULT '';
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS promoted boolean NOT NULL DEFAULT false;
-- the commission's reservation (holds.id), which also groups the commission's rows
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS hold_id bigint;
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS approval_id bigint;
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS accrued_hours integer NOT NULL DEFAULT 0;
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS accrued_eur numeric NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS cloud_resources_hold_idx ON cloud_resources (hold_id);
CREATE INDEX IF NOT EXISTS cloud_resources_repo_idx ON cloud_resources (repo);

-- a project's (repository owner/name) resource envelope: copied from the
-- policy's envelope_default on its first request, edited in the console
CREATE TABLE IF NOT EXISTS envelopes (
    project text PRIMARY KEY,
    monthly_eur numeric NOT NULL CHECK (monthly_eur >= 0),
    server_types jsonb NOT NULL,
    max_count integer NOT NULL CHECK (max_count >= 0),
    max_lease_hours integer NOT NULL CHECK (max_lease_hours >= 0),
    volume_max_gb integer NOT NULL CHECK (volume_max_gb >= 0),
    set_by text NOT NULL,
    set_at timestamptz NOT NULL DEFAULT now()
);
