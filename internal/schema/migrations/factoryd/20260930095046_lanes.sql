-- lanes (Stage 11): a card's lane and the process it follows, and a plan
-- card's key. Open agent rows take their lane from their status, and
-- rolls, report tasks and the human lane carry none. The loader fills
-- process.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS lane text CHECK (lane IN ('backlog', 'plan', 'ready')),
	ADD COLUMN IF NOT EXISTS process text,
	ADD COLUMN IF NOT EXISTS plan_key text;
UPDATE tasks SET status = 'backlog' WHERE status = 'suggested';
UPDATE tasks SET lane = CASE status WHEN 'backlog' THEN 'backlog' ELSE 'ready' END
	WHERE lane IS NULL AND kind = 'agent' AND parent_task IS NULL AND source <> 'roll'
	AND status IN ('backlog', 'queued', 'running', 'in-review', 'waiting-human', 'waiting-poster', 'closing');
-- a plan files many children of one parent: one report per source only
DROP INDEX IF EXISTS tasks_parent_source_key;
CREATE UNIQUE INDEX tasks_parent_source_key ON tasks (parent_task, source)
	WHERE parent_task IS NOT NULL AND source IN ('screening', 'presort');
CREATE UNIQUE INDEX IF NOT EXISTS tasks_plan_key ON tasks (parent_task, plan_key) WHERE source = 'plan';
