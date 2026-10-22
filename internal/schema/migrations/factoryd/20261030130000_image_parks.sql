-- a card parked on a sandbox image (request_tool, or an image with no pin
-- in force at its session's start), one row per task, its latest park:
-- target the image it waits for, tool and need what it asked for ('' for a
-- start whose image had no pin), tool_task the tool card that adds the
-- tool ('' for none), refused why no image comes ('' while one may),
-- woken_at when the imager signalled image_ready (a refusal at the park is
-- woken at once)
CREATE TABLE IF NOT EXISTS image_parks (
	task text PRIMARY KEY REFERENCES tasks(id),
	project text NOT NULL,
	target text NOT NULL,
	tool text NOT NULL DEFAULT '',
	need text NOT NULL DEFAULT '',
	tool_task text NOT NULL DEFAULT '',
	refused text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	woken_at timestamptz
);
CREATE INDEX IF NOT EXISTS image_parks_waiting ON image_parks (created_at) WHERE woken_at IS NULL;
CREATE INDEX IF NOT EXISTS image_parks_target ON image_parks (project, target) WHERE tool_task <> '';
