-- events (ruling 252): one row per action, whatever the input — a human's
-- button in the console, factoryd's own task events, an answer that arrived
-- as an issue comment, and later the project agent's chat. The project
-- event feed reads it by repository; a card's activity line its newest row.
CREATE TABLE IF NOT EXISTS events (
	id bigserial PRIMARY KEY,
	at timestamptz NOT NULL DEFAULT now(),
	repository text NOT NULL,
	task_id text,
	issue_number int,
	kind text NOT NULL,
	actor text NOT NULL,
	via text NOT NULL,
	detail jsonb
);

CREATE INDEX IF NOT EXISTS events_repository_id_idx ON events (repository, id);

-- NewestTaskEvents runs DISTINCT ON over task_id = ANY on every board
-- render; without this the read scans the table for each poll.
CREATE INDEX IF NOT EXISTS events_task_id_idx ON events (task_id, id);
