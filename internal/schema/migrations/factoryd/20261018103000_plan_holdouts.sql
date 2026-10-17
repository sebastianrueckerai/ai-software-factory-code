-- plan_holdouts (the spec-intake concept, piece 6): the hold-out criteria
-- a confirmation round proposed, one row per proposal, on the plan task.
-- A row holds its criterion, the drafted test, the dry run's head and
-- verdict (red on main is what a hold-out must be), the human's decision,
-- and what an audit session made of a red one at the gate. Stored rows
-- run at the criteria gate of the plan's integration card; the card's
-- comment never names them (blind by routing and storage).
CREATE TABLE IF NOT EXISTS plan_holdouts (
	task_id text NOT NULL,
	idx integer NOT NULL,
	round integer NOT NULL,
	id text NOT NULL,
	origin text NOT NULL,
	criterion text NOT NULL,
	state text NOT NULL,
	test_name text NOT NULL DEFAULT '',
	test_code text NOT NULL DEFAULT '',
	dry_head text NOT NULL DEFAULT '',
	dry_verdict text NOT NULL DEFAULT '',
	dry_output text NOT NULL DEFAULT '',
	audit_verdict text NOT NULL DEFAULT '',
	audit_reason text NOT NULL DEFAULT '',
	rewrite text NOT NULL DEFAULT '',
	red_cycles integer NOT NULL DEFAULT 0,
	decided_at timestamptz,
	PRIMARY KEY (task_id, idx)
);

CREATE INDEX IF NOT EXISTS plan_holdouts_round ON plan_holdouts (task_id, round);
