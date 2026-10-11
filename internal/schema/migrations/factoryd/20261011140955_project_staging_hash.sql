-- sha256 hex of the STAGING_URL and STAGING_SECRETS Actions secrets last set
ALTER TABLE projects ADD COLUMN IF NOT EXISTS staging_hash text NOT NULL DEFAULT ''
