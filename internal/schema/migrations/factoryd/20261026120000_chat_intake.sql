-- The intake switch (the chat header's): one row per repository, its
-- closed flag stopping exactly the roadmap pull (2026-11-01) — every
-- other filing still files; no row: the intake is open.

CREATE TABLE IF NOT EXISTS chat_intake (
	repository text PRIMARY KEY,
	closed boolean NOT NULL,
	set_by text NOT NULL,
	at timestamptz NOT NULL
);
