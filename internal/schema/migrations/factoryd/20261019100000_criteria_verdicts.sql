-- criteria_verdicts (the spec-intake concept, pieces 7 and 8, roadmap 290):
-- the verdict on one criterion of a plan, as data on the task that carries
-- the plan's criteria, the integration card. One row per source: the review
-- writes its rows every review, an approver's decision (the human
-- criterion's met at the merge approval, a waiver) writes the approver's
-- row, and the approver's row is the verdict that stands. Criterion ids
-- never change (roadmap 285), so the criterion_id is the join.
CREATE TABLE IF NOT EXISTS criteria_verdicts (
	task_id text NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
	criterion_id text NOT NULL,
	kind text NOT NULL,
	verdict text NOT NULL CHECK (verdict IN ('met', 'partial', 'unmet', 'waived', '')),
	evidence text NOT NULL DEFAULT '',
	fix text NOT NULL DEFAULT '' CHECK (fix IN ('card', 'planner', '')),
	source text NOT NULL CHECK (source IN ('review', 'approver')),
	decided_by text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (task_id, criterion_id, source)
);

-- the criteria gap approval (piece 8) needs its kind in the check
ALTER TABLE approval_requests DROP CONSTRAINT IF EXISTS approval_requests_kind_check;
ALTER TABLE approval_requests ADD CONSTRAINT approval_requests_kind_check CHECK (kind IN ('merge', 'resource', 'plan', 'criteria'));
