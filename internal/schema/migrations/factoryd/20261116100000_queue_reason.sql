-- A queued card carries why admission last refused it a work slot and
-- the time of the first refusal with that reason. The board digest and
-- the monitor read them, and admission clears both.
ALTER TABLE tasks ADD COLUMN queue_reason text NOT NULL DEFAULT '', ADD COLUMN queue_since timestamptz;
