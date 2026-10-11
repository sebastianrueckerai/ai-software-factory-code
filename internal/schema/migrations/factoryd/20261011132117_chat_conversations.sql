-- chat_messages gains a conversation number so a member may start a new
-- conversation while the old one stays readable. The default 1 keeps
-- every existing row in the first conversation. The index is replaced to
-- scope reads by conversation.
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS conversation bigint NOT NULL DEFAULT 1;

DROP INDEX IF EXISTS chat_messages_conversation_idx;
CREATE INDEX IF NOT EXISTS chat_messages_conversation_idx ON chat_messages (repository, login, conversation, id);
