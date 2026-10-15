-- a row of any writer waits for the turn before it as a member's second
-- message does (roadmap 263): queued is a state beside the row's kind, so
-- a poke and an answer's rows keep theirs while they wait.
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS queued boolean NOT NULL DEFAULT false;
UPDATE chat_messages SET queued = true, kind = '' WHERE kind = 'queued';
