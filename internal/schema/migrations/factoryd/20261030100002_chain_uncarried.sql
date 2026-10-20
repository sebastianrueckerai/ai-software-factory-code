-- a merged card the chain never carried: one open row per card, keyed by
-- fingerprint repository and id
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_kind_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_kind_check CHECK (kind IN ('gate_bound', 'supervisor_trip', 'escalation', 'info_request', 'alert', 'config_rejected', 'screening', 'month_cap', 'chain_red', 'chain_stalled', 'chain_uncarried'));
DROP INDEX IF EXISTS alerts_chain_key;
CREATE UNIQUE INDEX alerts_chain_key ON alerts (kind, fingerprint) WHERE kind IN ('chain_red', 'chain_stalled', 'chain_uncarried') AND verdict IS NULL;
-- a merged card carries its merge time, the clock of the uncarried alert:
-- every merged row without one first gets its gate_since, else its created_at,
-- and the check holds merged to it as it holds the final states
UPDATE tasks SET finished_at = COALESCE(gate_since, created_at)
WHERE status = 'merged' AND finished_at IS NULL;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_final_finished_at;
ALTER TABLE tasks ADD CONSTRAINT tasks_final_finished_at
	CHECK (status NOT IN ('done', 'failed', 'closed', 'merged') OR finished_at IS NOT NULL);
