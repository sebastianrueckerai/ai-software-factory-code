-- chat_proposals: the project agent's proposals waiting for their member's
-- Do it and the undos of applied actions, each a tool with its arguments,
-- bound to the member and the project, used once, until it expires, so a
-- restart of factoryd keeps them.
CREATE TABLE IF NOT EXISTS chat_proposals (
	token text PRIMARY KEY,
	kind text NOT NULL CHECK (kind IN ('proposal', 'undo')),
	repository text NOT NULL,
	login text NOT NULL,
	tool text NOT NULL,
	args jsonb NOT NULL,
	description text NOT NULL DEFAULT '',
	expires_at timestamptz NOT NULL,
	used_at timestamptz,
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS chat_proposals_member_idx ON chat_proposals (repository, login, expires_at);
