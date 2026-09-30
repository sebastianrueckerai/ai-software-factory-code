-- merge queue (Stage 11, merge: {queue: project}): one pull request of a
-- project merges at a time, and the oldest entry holds the turn.
CREATE TABLE merge_queue (
	project text NOT NULL,
	task_id text PRIMARY KEY REFERENCES tasks(id),
	entered_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX merge_queue_order ON merge_queue (project, entered_at, task_id);
