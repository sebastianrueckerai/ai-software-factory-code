-- The ask's bound action: an ask row can bind one of its choices to a tool
-- call, so the human's press runs it in code as their session. The binding
-- is a chat_proposals row of kind ask carrying the bound choice; the ask
-- row names its binding's token, so the settle rule and the tool park can
-- read whether it still stands.
ALTER TABLE chat_proposals DROP CONSTRAINT IF EXISTS chat_proposals_kind_check;
ALTER TABLE chat_proposals ADD CONSTRAINT chat_proposals_kind_check CHECK (kind IN ('proposal', 'undo', 'ask'));
ALTER TABLE chat_proposals ADD COLUMN IF NOT EXISTS choice text NOT NULL DEFAULT '';
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS proposal text NOT NULL DEFAULT '';
