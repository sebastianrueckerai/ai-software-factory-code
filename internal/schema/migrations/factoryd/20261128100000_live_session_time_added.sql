-- The agent time added to the card's cap that the session's deadline
-- counts, so a reattached session takes a raise made while factoryd was
-- away; null where an older factoryd wrote the row.
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS time_added_ns bigint;
