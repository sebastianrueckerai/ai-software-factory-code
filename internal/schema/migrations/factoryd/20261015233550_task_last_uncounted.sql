-- whether a task's last session got no model answer and took no attempt
-- (the session ladder): a restarted run's answer after one buys agent time
-- alone, as the run that asked would have
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS last_uncounted boolean NOT NULL DEFAULT false;
