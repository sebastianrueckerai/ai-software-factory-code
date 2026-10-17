-- plan staleness (the spec-intake concept, piece 10, roadmap 291): a
-- card's tasks row names the plan revision its first session ran
-- against, 0 while it has not started. The merge refuses a card whose
-- revision is past, and a new revision replaces the briefs of the
-- not-yet-started cards alone.
ALTER TABLE tasks ADD COLUMN plan_revision int NOT NULL DEFAULT 0;

-- the cards that already ran keep the revision they started against:
-- the newest one filed at or before their first session, 0 for a card
-- that never started.
UPDATE tasks t SET plan_revision = COALESCE((
	SELECT MAX(pr.revision) FROM plan_revisions pr
	WHERE pr.task_id = t.parent_task
	  AND pr.created_at <= (SELECT MIN(s.created_at) FROM sessions s WHERE s.task_id = t.id)
), 0)
WHERE t.source = 'plan';
