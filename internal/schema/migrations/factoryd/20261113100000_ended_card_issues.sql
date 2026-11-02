-- The issues of ended cards that stood open go to the heal pass once
-- (HealFactoryCloses), which confirms a closed issue without a write and
-- closes an open one, its dependencies on open issues dropped first.
-- factoryd's closes of closed cards stand unconfirmed again: Forgejo
-- refused the close of a card with open dependencies (412), and the heal
-- pass settled it. A merged factory-PR card of the agent lane gets the
-- close its done end now writes, and a closed plan the close it never
-- had, with no comment: its issue heard why already. A card or plan with
-- a close keeps it.
UPDATE intake_events SET confirmed_at = NULL
WHERE kind = 'factory_closed' AND confirmed_at IS NOT NULL
	AND task_id IN (SELECT id FROM tasks WHERE status = 'closed');

INSERT INTO intake_events (task_id, kind, author, body, comment_id)
SELECT id, 'factory_closed', 'factoryd', 'factoryd: pull request #' || pr_number || ' merged; this card ends with it.', issue_number
FROM tasks
WHERE kind = 'agent' AND source IN ('revert', 'github-sync', 'schema-copy', 'template-update')
	AND status IN ('done', 'merged') AND pr_number <> 0 AND issue_number <> 0
ON CONFLICT (task_id, kind, comment_id) WHERE comment_id <> 0 DO NOTHING;

INSERT INTO intake_events (task_id, kind, author, body, comment_id)
SELECT t.id, 'factory_closed', 'factoryd', '', t.issue_number
FROM tasks t
WHERE t.issue_number <> 0 AND EXISTS (SELECT 1 FROM events e WHERE e.task_id = t.id AND e.kind = 'plan_closed')
ON CONFLICT (task_id, kind, comment_id) WHERE comment_id <> 0 DO NOTHING;
