-- a chat row marked cut: an answer a member's stop ended mid-model-call,
-- which keeps the words that had arrived (the chat-stop plan). Unlike a
-- stop row it stays in the thread's context, replayed as any answer.
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS cut boolean NOT NULL DEFAULT false;
