-- a card's moments: the instants its objective names beside their UTC
-- words, which a screen shows in the reader's zone.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS moments jsonb;
