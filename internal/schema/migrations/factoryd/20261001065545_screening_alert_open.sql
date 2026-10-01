-- One open screening alert per task (item 132): the issue's screening and
-- each later answer's suspect verdict open their own once the earlier one
-- is judged, and a retried screening still finds its open row.
-- An image before this one opens at most one per task and runs unchanged.
-- Restoring the old index first needs every task's screening alerts but one
-- deleted.
DROP INDEX IF EXISTS alerts_screening_task_key;
CREATE UNIQUE INDEX alerts_screening_task_key ON alerts (task_id) WHERE kind = 'screening' AND verdict IS NULL;
