-- The freeze watcher reads the gateway's class_frozen and class_resumed
-- rows and its own class_woken rows among every card's events: a partial
-- index keeps those reads to the freeze rows.
CREATE INDEX IF NOT EXISTS events_freeze_rows_idx ON events (id) WHERE kind IN ('class_frozen', 'class_resumed', 'class_woken');
