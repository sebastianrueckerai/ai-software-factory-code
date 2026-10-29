-- a card has one report task per source: its screening and its sizing
DROP INDEX IF EXISTS tasks_parent_source_key;
CREATE UNIQUE INDEX tasks_parent_source_key ON tasks (parent_task, source)
	WHERE parent_task IS NOT NULL AND source IN ('screening', 'sizing');
