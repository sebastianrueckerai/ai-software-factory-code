-- task_deps (Stage 11): the cards a card of a plan waits on, filed by
-- CreateChildren with the edges of the plan. The dependency wait reads them.
CREATE TABLE IF NOT EXISTS task_deps (
	task_id text NOT NULL REFERENCES tasks(id),
	depends_on text NOT NULL REFERENCES tasks(id),
	PRIMARY KEY (task_id, depends_on),
	CHECK (task_id <> depends_on)
);
CREATE INDEX IF NOT EXISTS task_deps_depends_on ON task_deps (depends_on);
