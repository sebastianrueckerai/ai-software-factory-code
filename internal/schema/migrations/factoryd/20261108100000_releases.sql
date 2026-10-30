-- releases: each project's production hold (roadmap 336). Merged is not
-- live: while a plan's group is open on main, the project's production
-- waits. asks_at is when the project's CI last asked /release (null:
-- never); held_since the hold's clock (null: not held); asked_at the
-- hold's last ask to a person; told_held the newest commit a promote job
-- was told held ('' for none); deploy_now the commit an approver's Deploy
-- now released ('' for none).
CREATE TABLE IF NOT EXISTS releases (
	repository text PRIMARY KEY,
	asks_at timestamptz,
	held_since timestamptz,
	asked_at timestamptz,
	told_held text NOT NULL DEFAULT '',
	deploy_now text NOT NULL DEFAULT ''
);
