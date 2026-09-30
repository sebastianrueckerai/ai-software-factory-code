-- the month cap's spend alerts (item 64 a): one row per UTC month and
-- percent of budget.month_eur, keyed by fingerprint month/percent
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_kind_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_kind_check CHECK (kind IN ('gate_bound', 'supervisor_trip', 'escalation', 'info_request', 'alert', 'config_rejected', 'screening', 'month_cap'));
CREATE UNIQUE INDEX IF NOT EXISTS alerts_month_cap_key ON alerts (fingerprint) WHERE kind = 'month_cap';
