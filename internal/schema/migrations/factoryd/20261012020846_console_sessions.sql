-- console_sessions: the console's logins, so a member survives a restart
-- of factoryd (roadmap 164). id_hash is the SHA-256 hex of the cookie's
-- id, never the id itself; access and refresh hold the member's Forgejo
-- OAuth tokens encrypted with AES-256-GCM under a key derived from the
-- console's session key, never in clear.
CREATE TABLE IF NOT EXISTS console_sessions (
	id_hash text PRIMARY KEY,
	login text NOT NULL,
	csrf text NOT NULL,
	access text NOT NULL DEFAULT '',
	refresh text NOT NULL DEFAULT '',
	token_expiry timestamptz,
	created_at timestamptz NOT NULL DEFAULT now(),
	last_seen timestamptz NOT NULL DEFAULT now(),
	expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS console_sessions_expires_at_idx ON console_sessions (expires_at);
