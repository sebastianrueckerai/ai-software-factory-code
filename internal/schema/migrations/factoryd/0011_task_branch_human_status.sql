ALTER TABLE tasks DROP COLUMN IF EXISTS branch;
UPDATE tasks SET status = 'waiting-human' WHERE kind = 'human' AND status = 'queued';
