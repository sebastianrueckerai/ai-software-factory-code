-- The freeze watcher lists the reopens it owes Forgejo every minute
-- (intake_events of kind factory_reopened): a partial index keeps that
-- read to the few rows that stand.
CREATE INDEX IF NOT EXISTS intake_events_owed_reopen_idx ON intake_events (id) WHERE kind = 'factory_reopened';
