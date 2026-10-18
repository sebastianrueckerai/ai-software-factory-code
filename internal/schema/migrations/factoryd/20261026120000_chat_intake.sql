-- The intake switch (the chat header's): one row per repository, its
-- closed flag refusing the wake's own filings — the turns the Waker
-- starts without the member's ask; no row: the intake is open.

CREATE TABLE IF NOT EXISTS chat_intake (
	repository text PRIMARY KEY,
	closed boolean NOT NULL,
	set_by text NOT NULL,
	at timestamptz NOT NULL
);
