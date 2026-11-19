-- The text each session's first model call read, as its worker built it,
-- so the card shows a human what each session got. session is the
-- session's number among its card's sessions, from 1, oldest first.
CREATE TABLE IF NOT EXISTS session_briefs (
    task_id text NOT NULL REFERENCES tasks(id),
    session integer NOT NULL,
    brief text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, session)
);
