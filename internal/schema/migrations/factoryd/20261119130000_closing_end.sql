-- The move to closing stores the end the card's run decided, so a run
-- restarted at closing finishes with it.
ALTER TABLE tasks
  ADD COLUMN closing_final text NOT NULL DEFAULT '',
  ADD COLUMN closing_reason text NOT NULL DEFAULT '';
