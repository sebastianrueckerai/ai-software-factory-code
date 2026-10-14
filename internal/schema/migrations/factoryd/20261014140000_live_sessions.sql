-- A running agent session's sandbox (roadmap 222, item 2): the pod a
-- factoryd that restarts reattaches to, instead of starting the attempt
-- over, and how many of the pod's log lines the session loop consumed.
CREATE TABLE live_sessions (
	task_id text PRIMARY KEY REFERENCES tasks(id),
	session_id text NOT NULL,
	namespace text NOT NULL,
	pod text NOT NULL,
	message_token text NOT NULL,
	started_at timestamptz NOT NULL,
	deadline timestamptz NOT NULL,
	lines integer NOT NULL DEFAULT 0
);
