-- chat_messages (stage 15 phase 4a): a member's conversation with the
-- project agent, per project and member. A user row holds the member's
-- text, an assistant row the agent's text and in tool_calls each call it
-- made with its result, so a later turn replays a valid conversation.
CREATE TABLE IF NOT EXISTS chat_messages (
	id bigserial PRIMARY KEY,
	repository text NOT NULL,
	login text NOT NULL,
	role text NOT NULL CHECK (role IN ('user', 'assistant')),
	content text NOT NULL DEFAULT '',
	tool_calls jsonb,
	at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS chat_messages_conversation_idx ON chat_messages (repository, login, id);
