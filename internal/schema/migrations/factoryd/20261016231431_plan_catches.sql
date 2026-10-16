-- plan_catches (the spec-intake concept's recording rule, roadmap 285):
-- the holes a plan revision's making caught, one row per catch, keyed by
-- kind and finder. The first kind is question, its finder the planner,
-- written when the revision's questions are stored; the events
-- (question_asked) stay the trail, this table the ledger a later step
-- reads back.
CREATE TABLE IF NOT EXISTS plan_catches (
	task_id text NOT NULL,
	revision int NOT NULL,
	kind text NOT NULL,
	idx int NOT NULL,
	finder text NOT NULL,
	detail text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (task_id, revision, kind, idx),
	FOREIGN KEY (task_id, revision) REFERENCES plan_revisions (task_id, revision)
);
