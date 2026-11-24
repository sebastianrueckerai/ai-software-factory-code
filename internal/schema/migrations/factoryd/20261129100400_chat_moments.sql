-- a chat row's moments: the instants its content names beside their UTC
-- words, which a screen shows in the reader's zone.
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS moments jsonb;
