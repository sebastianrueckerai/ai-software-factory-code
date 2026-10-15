-- The routines' state and runs (roadmap 267). A routine is configuration in the tree. The state row counts merges
-- since the last merge-triggered run and holds a pending Skip once. A run row is written when a run begins and
-- finished when its card ends. The key is the project file's stem, and the factory's own routines of policy.yaml
-- use the empty project.
CREATE TABLE routine_state (
  project text NOT NULL,
  routine text NOT NULL,
  merges integer NOT NULL DEFAULT 0,
  skip_next boolean NOT NULL DEFAULT false,
  skip_by text NOT NULL DEFAULT '',
  runs integer NOT NULL DEFAULT 0,
  PRIMARY KEY (project, routine)
);
CREATE TABLE routine_runs (
  project text NOT NULL,
  routine text NOT NULL,
  run integer NOT NULL,
  due timestamptz NOT NULL,
  started timestamptz NOT NULL DEFAULT now(),
  ended timestamptz,
  trigger text NOT NULL,
  task_id text NOT NULL DEFAULT '',
  issue_number bigint NOT NULL DEFAULT 0,
  outcome text NOT NULL DEFAULT '',
  note text NOT NULL DEFAULT '',
  cost_eur numeric NOT NULL DEFAULT 0,
  PRIMARY KEY (project, routine, run)
);
CREATE UNIQUE INDEX routine_runs_due ON routine_runs (project, routine, due);
CREATE INDEX routine_runs_task ON routine_runs (task_id) WHERE task_id <> '';
-- The merge pass's cursor is the last event it read. It starts at the newest event, so the merges before the
-- routines count for none.
CREATE TABLE routine_merge_cursor (
  one boolean PRIMARY KEY DEFAULT true CHECK (one),
  last_event_id bigint NOT NULL DEFAULT 0
);
INSERT INTO routine_merge_cursor (last_event_id) SELECT coalesce(max(id), 0) FROM events;
