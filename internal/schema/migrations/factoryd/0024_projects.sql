-- a project is a repository the factory works on, the namespaces it
-- deploys into and its settings (ruling 146). Managed rows get their
-- Forgejo and cluster objects from factoryd (ruling 156), the factory's
-- own repositories keep theirs in the infra repository
CREATE TABLE IF NOT EXISTS projects (
    name text PRIMARY KEY CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
    repository text NOT NULL UNIQUE CHECK (repository ~ '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'),
    merge_rule text NOT NULL DEFAULT 'human' CHECK (merge_rule IN ('auto', 'human')),
    -- empty: the policy's approvers
    notify text[] NOT NULL DEFAULT '{}',
    namespaces text[] NOT NULL DEFAULT '{}',
    -- [{namespace, path, hostname}]: one Flux Kustomization each (ruling 157)
    environments jsonb NOT NULL DEFAULT '[]',
    quota_cpu text NOT NULL DEFAULT '',
    quota_memory text NOT NULL DEFAULT '',
    quota_storage text NOT NULL DEFAULT '',
    managed boolean NOT NULL DEFAULT true,
    -- sha256 of FACTORY_READ_TOKEN, hex
    read_token_hash text NOT NULL DEFAULT '',
    -- factory-ci's token limited to this repository: its Actions secrets
    -- and runners, which Forgejo keeps to the owner (minted at registration)
    ci_token text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO projects (name, repository, namespaces, managed) VALUES
    ('ai-software-factory-code', 'sf/ai-software-factory-code', '{factory}', false),
    ('ai-software-factory-infra', 'sf/ai-software-factory-infra', '{}', false)
ON CONFLICT DO NOTHING;

-- two resource kinds (ruling 147): a lease ends with its task or its
-- lease, a standing resource lives until a human releases it
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS lifetime text NOT NULL DEFAULT 'lease'
    CHECK (lifetime IN ('lease', 'standing'));
UPDATE cloud_resources SET lifetime = 'standing' WHERE promoted;
ALTER TABLE cloud_resources DROP COLUMN IF EXISTS promoted;

ALTER TABLE envelopes ADD COLUMN IF NOT EXISTS standing_monthly_eur numeric NOT NULL DEFAULT 0 CHECK (standing_monthly_eur >= 0);
ALTER TABLE envelopes ADD COLUMN IF NOT EXISTS standing_server_types jsonb NOT NULL DEFAULT '[]';
ALTER TABLE envelopes ADD COLUMN IF NOT EXISTS standing_max_count integer NOT NULL DEFAULT 0 CHECK (standing_max_count >= 0);
