-- plan_proofs: the plan's blind proofs, one row each: a criterion's test
-- the prover wrote, or a hold-out the confirmation proposed, a claim with
-- inputs and expected numbers until the prover files its test. The code
-- lives here and nowhere else. The audit and the dry run at confirmation
-- are gone, and their columns with them.
ALTER TABLE plan_holdouts RENAME TO plan_proofs;
ALTER INDEX plan_holdouts_round RENAME TO plan_proofs_round;
ALTER TABLE plan_proofs RENAME COLUMN criterion TO claim;
ALTER TABLE plan_proofs RENAME COLUMN test_name TO name;
ALTER TABLE plan_proofs RENAME COLUMN test_code TO code;
ALTER TABLE plan_proofs
	ADD COLUMN criterion text NOT NULL DEFAULT '',
	ADD COLUMN inputs text NOT NULL DEFAULT '',
	ADD COLUMN expected text NOT NULL DEFAULT '',
	ADD COLUMN path text NOT NULL DEFAULT '',
	ADD COLUMN suite text NOT NULL DEFAULT '',
	ADD COLUMN artifact text NOT NULL DEFAULT '',
	ADD COLUMN flag text NOT NULL DEFAULT '',
	DROP COLUMN dry_head,
	DROP COLUMN dry_verdict,
	DROP COLUMN dry_output,
	DROP COLUMN audit_verdict,
	DROP COLUMN audit_reason,
	DROP COLUMN rewrite,
	DROP COLUMN red_cycles;
-- every row so far is a hold-out drafted with its code at confirmation:
-- the code goes, a human's okay stands and the prover writes the test, an
-- unrunnable draft is dropped
UPDATE plan_proofs SET origin = 'hold-out', name = '', code = '',
	state = CASE state WHEN 'unrunnable' THEN 'dropped' WHEN 'stored' THEN 'okayed' WHEN 'opened' THEN 'okayed' ELSE state END;
ALTER TABLE plan_proofs ADD CONSTRAINT plan_proofs_state_check CHECK (state IN ('proposed', 'okayed', 'dropped', 'filed', 'stored', 'opened'));
ALTER TABLE plan_proofs ADD CONSTRAINT plan_proofs_origin_check CHECK ((origin = 'hold-out' AND criterion = '') OR (origin = 'criterion' AND criterion = id));
ALTER TABLE plan_proofs ADD CONSTRAINT plan_proofs_flag_check CHECK (flag IN ('', 'green on main', 'unrunnable'));
ALTER TABLE plan_proofs ADD CONSTRAINT plan_proofs_file_check CHECK (state NOT IN ('filed', 'stored', 'opened') OR (path <> '' AND suite <> '' AND name <> '' AND code <> ''));
CREATE UNIQUE INDEX IF NOT EXISTS plan_proofs_criterion ON plan_proofs (task_id, criterion) WHERE criterion <> '';
