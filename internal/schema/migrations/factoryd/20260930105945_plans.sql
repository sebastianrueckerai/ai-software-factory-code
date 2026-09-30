-- plans (Stage 11): every valid plan a planner answered is the plan task's
-- next revision, and an approver accepts one through an approval request
-- of kind plan. The kind check is rewritten whole (TestKindChecksAccept).
CREATE TABLE IF NOT EXISTS plan_revisions (
	task_id text NOT NULL REFERENCES tasks(id),
	revision int NOT NULL,
	plan jsonb NOT NULL,
	sha256 text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (task_id, revision)
);
ALTER TABLE approval_requests DROP CONSTRAINT IF EXISTS approval_requests_kind_check;
ALTER TABLE approval_requests ADD CONSTRAINT approval_requests_kind_check CHECK (kind IN ('merge', 'resource', 'plan'));
