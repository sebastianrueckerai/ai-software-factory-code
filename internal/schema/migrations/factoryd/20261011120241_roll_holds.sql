-- roll_holds (stage 16, ruling 269): a roll task whose merge waits for a gap
-- between agent sessions holds new sessions from since until until, or until
-- its merge ends or the task does. The newest hold in force is the one read.
CREATE TABLE IF NOT EXISTS roll_holds (
	task_id text PRIMARY KEY,
	since timestamptz NOT NULL,
	until timestamptz NOT NULL
);
