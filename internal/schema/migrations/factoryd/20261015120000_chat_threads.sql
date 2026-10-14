-- chat_conversations becomes one row per conversation (roadmap 239): a
-- title from the thread's first message, the active/archived state, the
-- running summary compaction keeps, the inbox flag, and the member's
-- current pointer as one row's current = true. Old rows keep their
-- numbers; a member's pointer with no messages yet becomes an empty
-- current row. The inbox thread is lazy-created, none backfilled.
CREATE TABLE IF NOT EXISTS chat_conversations_new (
	repository text NOT NULL,
	login text NOT NULL,
	conversation bigint NOT NULL,
	title text NOT NULL DEFAULT '',
	state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'archived')),
	inbox boolean NOT NULL DEFAULT false,
	current boolean NOT NULL DEFAULT false,
	created_at timestamptz NOT NULL DEFAULT now(),
	last_turn_at timestamptz,
	last_turn_tokens integer NOT NULL DEFAULT 0,
	summary text NOT NULL DEFAULT '',
	summary_at timestamptz,
	summary_model text NOT NULL DEFAULT '',
	compact_count integer NOT NULL DEFAULT 0,
	summary_upto bigint NOT NULL DEFAULT 0,
	compact_tokens_before integer NOT NULL DEFAULT 0,
	compact_tokens_after integer NOT NULL DEFAULT 0,
	PRIMARY KEY (repository, login, conversation)
);

INSERT INTO chat_conversations_new (repository, login, conversation, title, created_at, last_turn_at, current)
SELECT repository, login, conversation,
	coalesce(left(btrim(regexp_replace((array_agg(content ORDER BY id) FILTER (WHERE role = 'user' AND content <> ''))[1], '\s+', ' ', 'g')), 80), ''),
	min(at), max(at),
	(conversation = COALESCE((SELECT old.current FROM chat_conversations old
		WHERE old.repository = m.repository AND old.login = m.login), 1))
FROM chat_messages m
GROUP BY repository, login, conversation;

INSERT INTO chat_conversations_new (repository, login, conversation, title, created_at, current)
SELECT old.repository, old.login, old.current, '', now(), true
FROM chat_conversations old
WHERE NOT EXISTS (SELECT 1 FROM chat_messages m
	WHERE m.repository = old.repository AND m.login = old.login AND m.conversation = old.current);

DROP TABLE chat_conversations;
ALTER TABLE chat_conversations_new RENAME TO chat_conversations;

-- one current thread and one inbox per member and project
CREATE UNIQUE INDEX IF NOT EXISTS chat_conversations_current_idx ON chat_conversations (repository, login) WHERE current;
CREATE UNIQUE INDEX IF NOT EXISTS chat_conversations_inbox_idx ON chat_conversations (repository, login) WHERE inbox;

-- a proposal and an undo belong to the thread that raised them: their Do
-- it's and their undo's rows land there; 0 for one stored before
ALTER TABLE chat_proposals ADD COLUMN IF NOT EXISTS conversation bigint NOT NULL DEFAULT 0;

-- a message row names the kind of agent row factoryd wrote itself (a
-- deterministic poke, an escalation's face) and the choices it offers as
-- buttons, where a decision fits (roadmap 239).
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT '';
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS choices jsonb;
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS decider text NOT NULL DEFAULT '';

-- the seen marker per member and thread: a turn that ends after it marks
-- the thread unread; opening it clears (roadmap 239).
CREATE TABLE IF NOT EXISTS chat_seen (
	repository text NOT NULL,
	login text NOT NULL,
	conversation bigint NOT NULL,
	last_seen_message_id bigint NOT NULL DEFAULT 0,
	PRIMARY KEY (repository, login, conversation)
);

DROP INDEX IF EXISTS chat_messages_conversation_idx;
CREATE INDEX IF NOT EXISTS chat_messages_conversation_idx ON chat_messages (repository, login, conversation, id);
