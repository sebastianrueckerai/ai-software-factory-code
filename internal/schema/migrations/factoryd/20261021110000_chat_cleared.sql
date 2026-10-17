-- the cleared marker per member and thread (the chat's Clear old): the
-- console's sweep of the old rows from view, the record keeps them. It
-- rides the seen marker's table; 0 sweeps nothing.
ALTER TABLE chat_seen ADD COLUMN IF NOT EXISTS cleared_message_id bigint NOT NULL DEFAULT 0;
