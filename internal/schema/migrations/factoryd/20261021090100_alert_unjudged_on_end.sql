-- a SigNoz alert whose task ended without a verdict is unjudged
UPDATE alerts SET verdict = 'unjudged', resolved_by = 'factoryd', resolved_at = now()
WHERE kind = 'alert' AND verdict IS NULL
  AND task_id IN (SELECT id FROM tasks WHERE status IN ('done', 'failed', 'closed'));
