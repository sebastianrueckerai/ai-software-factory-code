-- chat_messages gains the number of the member's conversation a row belongs
-- to, so a member may start a new conversation while the old ones stay
-- readable: every earlier row is in conversation 1. chat_conversations
-- holds the conversation a member started last on a project, which has no
-- rows until their first message.
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS conversation bigint NOT NULL DEFAULT 1;

DROP INDEX IF EXISTS chat_messages_conversation_idx;
CREATE INDEX IF NOT EXISTS chat_messages_conversation_idx ON chat_messages (repository, login, conversation, id);

CREATE TABLE IF NOT EXISTS chat_conversations (
	repository text NOT NULL,
	login text NOT NULL,
	current bigint NOT NULL,
	PRIMARY KEY (repository, login)
);
