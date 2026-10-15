-- a task that ended carries its finish time (roadmap 271): every final
-- row without one — ended by an operator's SQL or a write that never
-- stamped it, like t-7387c9e3b193 (#568) — first gets its gate_since,
-- else its created_at, and the check refuses from then on any write that
-- ends a task without its stamp, so the board's finished columns, which
-- read finished_at, lose no card again
UPDATE tasks SET finished_at = COALESCE(gate_since, created_at)
WHERE status IN ('done', 'failed', 'closed') AND finished_at IS NULL;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_final_finished_at;
ALTER TABLE tasks ADD CONSTRAINT tasks_final_finished_at
	CHECK (status NOT IN ('done', 'failed', 'closed') OR finished_at IS NOT NULL);
