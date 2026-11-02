CREATE TABLE chat_tokens (
	id bigserial PRIMARY KEY,
	hash text NOT NULL UNIQUE,
	login text NOT NULL,
	repository text NOT NULL,
	name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
	sealed text NOT NULL,
	conversation bigint NOT NULL DEFAULT 0,
	created_at timestamptz NOT NULL DEFAULT now(),
	expires_at timestamptz NOT NULL,
	last_used timestamptz,
	revoked_at timestamptz
);
CREATE INDEX chat_tokens_login ON chat_tokens (login, created_at DESC);
ALTER TABLE chat_messages ADD COLUMN token text NOT NULL DEFAULT '';
