-- a merged card the chain never carried: one open row per card, keyed by
-- fingerprint repository and id
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_kind_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_kind_check CHECK (kind IN ('gate_bound', 'supervisor_trip', 'escalation', 'info_request', 'alert', 'config_rejected', 'screening', 'month_cap', 'chain_red', 'chain_stalled', 'chain_uncarried'));
DROP INDEX IF EXISTS alerts_chain_key;
CREATE UNIQUE INDEX alerts_chain_key ON alerts (kind, fingerprint) WHERE kind IN ('chain_red', 'chain_stalled', 'chain_uncarried') AND verdict IS NULL;
