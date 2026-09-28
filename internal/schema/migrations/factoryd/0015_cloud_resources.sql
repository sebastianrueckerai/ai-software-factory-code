CREATE TABLE IF NOT EXISTS cloud_resources (
    id bigserial PRIMARY KEY,
    project text NOT NULL CHECK (project IN ('ci', 'agent')),
    kind text NOT NULL,
    provider_id text NOT NULL DEFAULT '',
    name text NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}',
    owner_ref text NOT NULL,
    price_per_hour_eur numeric NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    lease_expires timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'requested'
        CHECK (state IN ('requested', 'live', 'deleting', 'deleted', 'vanished')),
    deleted_at timestamptz
);

CREATE INDEX IF NOT EXISTS cloud_resources_open_idx
    ON cloud_resources (project, owner_ref) WHERE state IN ('requested', 'live', 'deleting');
