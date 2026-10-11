-- plan_questions (Stage 16 item 3.5, ruling 264): the open points a plan
-- revision asks the member, numbered from 1, each with the planner's
-- choices and the member's answer, NULL until given. A revision with an
-- open question is not accepted.
CREATE TABLE IF NOT EXISTS plan_questions (
	task_id text NOT NULL,
	revision int NOT NULL,
	idx int NOT NULL,
	text text NOT NULL,
	choices jsonb NOT NULL,
	answer text,
	answered_by text,
	answered_at timestamptz,
	PRIMARY KEY (task_id, revision, idx),
	FOREIGN KEY (task_id, revision) REFERENCES plan_revisions (task_id, revision)
);
