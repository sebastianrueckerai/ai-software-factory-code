-- the analytics (ruling 185): the alias of a task's last session, and when
-- the daily pass found its merged work reverted
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS model_alias text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS reverted_at timestamptz;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS model text NOT NULL DEFAULT '';
