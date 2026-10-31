-- sha256 hex of the FORGEJO_SSH_HOST_KEY Actions secret last set
ALTER TABLE projects ADD COLUMN IF NOT EXISTS host_key_hash text NOT NULL DEFAULT ''
