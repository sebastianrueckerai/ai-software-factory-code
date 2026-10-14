-- the scratchpad (roadmap 239, the 2026-10-13 plan's working notes): one
-- per project, open to every thread of its chat; it holds state and
-- steers nothing.
CREATE TABLE IF NOT EXISTS chat_notes (
	repository text PRIMARY KEY,
	content text NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now(),
	updated_by text NOT NULL
);
