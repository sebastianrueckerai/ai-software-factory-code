-- The agent time added to the card's cap that the session's deadline
-- counts, and the card's budget the session last took, so a reattached
-- session takes a raise or a fund made while factoryd was away; null where
-- an older factoryd wrote the row.
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS time_added_ns bigint,
	ADD COLUMN IF NOT EXISTS budget_eur double precision;
