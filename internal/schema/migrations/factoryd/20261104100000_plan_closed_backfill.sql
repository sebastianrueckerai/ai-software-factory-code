-- plan_closed for the plans that ended before a plan without criteria
-- could close (roadmap 336): each plan card whose newest revision holds no
-- criteria and whose cards (source plan, holding a key) are all done or
-- closed gets its close row, quietly, with no issue comment. The open
-- groups read the row, so an old plan holds nothing.
INSERT INTO events (repository, task_id, issue_number, kind, actor, via, detail)
SELECT p.repository, p.id, NULLIF(p.issue_number, 0), 'plan_closed', 'factoryd', 'factoryd', '{"criteria": 0, "backfill": true}'::jsonb
FROM tasks p
WHERE p.track = 'plan'
	AND EXISTS (SELECT 1 FROM tasks c WHERE c.parent_task = p.id AND c.source = 'plan' AND c.plan_key IS NOT NULL)
	AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_task = p.id AND c.source = 'plan' AND c.plan_key IS NOT NULL
		AND c.status NOT IN ('done', 'closed'))
	AND NOT EXISTS (SELECT 1 FROM plan_revisions r WHERE r.task_id = p.id
		AND r.revision = (SELECT max(revision) FROM plan_revisions WHERE task_id = p.id)
		AND jsonb_typeof(r.plan->'criteria') = 'array' AND r.plan->'criteria' <> '[]'::jsonb)
	AND NOT EXISTS (SELECT 1 FROM events e WHERE e.task_id = p.id AND e.kind = 'plan_closed')
