-- merge_receipts (roadmap 66): one receipt per merged task, the
-- deterministic facts of its merge — the base and head SHAs, the diff's
-- hash, the policy version, every gate's command, exit code and verdict,
-- the cost, every human decision — apart from the model's assessments,
-- which the document carries in its own labelled section. The first
-- write stands (ON CONFLICT DO NOTHING): a merge path that runs again —
-- the merge queue, the merge webhook — writes nothing over the receipt
-- it already wrote. The document is versioned: the compliance evidence
-- export (roadmap 328) reads it by that version.
CREATE TABLE IF NOT EXISTS merge_receipts (
	task_id text PRIMARY KEY REFERENCES tasks (id) ON DELETE CASCADE,
	document jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);
