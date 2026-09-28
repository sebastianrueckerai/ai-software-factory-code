ALTER TABLE tasks ADD COLUMN IF NOT EXISTS required_capabilities jsonb NOT NULL DEFAULT '[]';
ALTER TABLE audit ADD COLUMN IF NOT EXISTS policy_version_id bigint;
