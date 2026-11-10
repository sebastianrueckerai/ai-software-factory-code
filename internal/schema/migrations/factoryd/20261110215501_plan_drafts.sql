-- The plan a planner's refused filing left, with the findings it was
-- refused for: one per plan task, replaced by each refusal and deleted when
-- a revision is stored. Never a revision.
CREATE TABLE IF NOT EXISTS plan_drafts (
	task_id text PRIMARY KEY REFERENCES tasks(id),
	plan jsonb NOT NULL,
	findings jsonb NOT NULL,
	at timestamptz NOT NULL DEFAULT now()
);
