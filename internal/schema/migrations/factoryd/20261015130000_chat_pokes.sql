-- factoryd's pokes (roadmap 239): a poke row names the card it is about,
-- and a repeat of the same poke counts on its row instead of adding one
-- (74's rule). The cursor is the last event the poke writer read; its
-- first pass starts at the newest event, so no history pokes.
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS task_id text NOT NULL DEFAULT '';
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS repeats integer NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS chat_poke_cursor (
	one boolean PRIMARY KEY DEFAULT true CHECK (one),
	event_id bigint NOT NULL
);
