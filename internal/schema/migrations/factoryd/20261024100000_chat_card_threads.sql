-- A card's own chat thread (the main agent concept): one per card,
-- whichever member opens its door first binds it.

ALTER TABLE chat_conversations ADD COLUMN task_id text;

CREATE UNIQUE INDEX chat_conversations_card ON chat_conversations (repository, task_id) WHERE task_id IS NOT NULL;
