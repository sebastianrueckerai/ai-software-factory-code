-- The inbox is the factory's thread, never a human's current one: a
-- reload lands on the current thread, and the inbox takes no typed
-- message. A row that holds it current loses the flag, so
-- CurrentConversation answers one past the human's highest thread, and the
-- check keeps every later write from setting it.
UPDATE chat_conversations SET current = false WHERE inbox AND current;
ALTER TABLE chat_conversations DROP CONSTRAINT IF EXISTS chat_conversations_inbox_never_current;
ALTER TABLE chat_conversations ADD CONSTRAINT chat_conversations_inbox_never_current CHECK (NOT (inbox AND current));
