-- The Waker's rows (the main agent concept): the cursor is the last event
-- a repository's pass handled, planted at the newest event on its first
-- read so no history wakes, and the marks are what a pass did, so a
-- restart or a repeat changes nothing.

CREATE TABLE IF NOT EXISTS chat_wake_cursor (
	repository text PRIMARY KEY,
	event_id bigint NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_wake_state (
	repository text NOT NULL,
	key text NOT NULL,
	at timestamptz NOT NULL,
	PRIMARY KEY (repository, key)
);
