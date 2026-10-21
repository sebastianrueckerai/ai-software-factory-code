-- when factoryd finished a close it recorded as its own (a factory_closed
-- row, its comment in body): NULL while the comment or the close is still
-- to be done, which the heal pass finishes. A close recorded before this
-- column ended in the old order and counts as confirmed.
ALTER TABLE intake_events ADD COLUMN IF NOT EXISTS confirmed_at timestamptz;
UPDATE intake_events SET confirmed_at = created_at WHERE kind = 'factory_closed' AND confirmed_at IS NULL;
