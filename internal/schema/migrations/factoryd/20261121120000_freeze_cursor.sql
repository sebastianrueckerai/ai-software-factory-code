-- The freeze card watcher's cursor on the shared events table: the id of
-- the last class_frozen or class_resumed row it read, so a restart reads
-- forward only. One row, id 1.
CREATE TABLE IF NOT EXISTS freeze_cursor (
	id int PRIMARY KEY,
	cursor bigint NOT NULL DEFAULT 0
);

INSERT INTO freeze_cursor (id, cursor) VALUES (1, 0) ON CONFLICT DO NOTHING;
