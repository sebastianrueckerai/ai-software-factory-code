-- A card back from a wait that the surge bound keeps from a session waits
-- queued with the time its resumption began. It holds no work slot, and
-- the queue admits these cards before the ones never started.
ALTER TABLE tasks ADD COLUMN resumed_at timestamptz;
