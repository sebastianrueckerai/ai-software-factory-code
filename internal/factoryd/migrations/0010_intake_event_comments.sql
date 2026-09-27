ALTER TABLE intake_events ADD COLUMN IF NOT EXISTS comment_id bigint NOT NULL DEFAULT 0;
ALTER TABLE intake_events ADD COLUMN IF NOT EXISTS trusted boolean NOT NULL DEFAULT false;
ALTER TABLE intake_events ADD COLUMN IF NOT EXISTS signalled boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX IF NOT EXISTS intake_events_comment_key ON intake_events (task_id, kind, comment_id) WHERE comment_id <> 0;
