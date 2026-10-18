-- The timers and watches (the main agent concept): a member's thread
-- stores its own subscription on a card, whose moves wake the thread,
-- and its one-shot timers, whose due rows the Waker takes, wakes the
-- thread with its note and deletes. A watch is one per member and card;
-- a timer's task, when set, binds it to the card so the card's end takes
-- the timer with the watch.

CREATE TABLE IF NOT EXISTS chat_watches (
	id bigserial PRIMARY KEY,
	repository text NOT NULL,
	login text NOT NULL,
	conversation bigint NOT NULL,
	task_id text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (repository, login, task_id)
);

CREATE TABLE IF NOT EXISTS chat_timers (
	id bigserial PRIMARY KEY,
	repository text NOT NULL,
	login text NOT NULL,
	conversation bigint NOT NULL,
	due_at timestamptz NOT NULL,
	note text NOT NULL,
	task_id text,
	created_at timestamptz NOT NULL DEFAULT now()
);
