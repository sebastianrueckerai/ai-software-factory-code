-- A project's merge queue pauses while main is red (roadmap 222, item 4):
-- the push run of main that failed, the fix-forward card the roller filed
-- for it, and the revert card it files when no fix lands within the bound.
CREATE TABLE queue_pauses (
	project text PRIMARY KEY,
	run_id bigint NOT NULL,
	sha text NOT NULL,
	since timestamptz NOT NULL,
	fix_task text NOT NULL DEFAULT '',
	revert_task text NOT NULL DEFAULT ''
);
