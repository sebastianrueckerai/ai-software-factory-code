-- tasks_parent_source_key: a card has one report task per source, its
-- screening and its sizing; no code files a presort any more.
-- A rollback to an image before this one first needs:
--   DROP INDEX tasks_parent_source_key;
--   CREATE UNIQUE INDEX tasks_parent_source_key ON tasks (parent_task, source)
--     WHERE parent_task IS NOT NULL AND source IN ('screening', 'presort');
DROP INDEX IF EXISTS tasks_parent_source_key;
CREATE UNIQUE INDEX tasks_parent_source_key ON tasks (parent_task, source)
	WHERE parent_task IS NOT NULL AND source IN ('screening', 'sizing');
