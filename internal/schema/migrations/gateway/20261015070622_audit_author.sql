-- audit gains the human behind its task (roadmap 41): the task's issue
-- author, read from tasks.author as each row is written; '' for a call of
-- no known task and on the rows from before.
ALTER TABLE audit ADD COLUMN IF NOT EXISTS author text NOT NULL DEFAULT '';
