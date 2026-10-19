-- the chain poller's rules: a repository's red or stalled head, one open
-- row per project, keyed by fingerprint the repository
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_kind_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_kind_check CHECK (kind IN ('gate_bound', 'supervisor_trip', 'escalation', 'info_request', 'alert', 'config_rejected', 'screening', 'month_cap', 'chain_red', 'chain_stalled'));
CREATE UNIQUE INDEX IF NOT EXISTS alerts_chain_key ON alerts (kind, fingerprint) WHERE kind IN ('chain_red', 'chain_stalled') AND verdict IS NULL;
