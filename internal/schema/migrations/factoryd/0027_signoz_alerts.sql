-- a SigNoz alert in a project's namespace, filed as a bug issue: one row
-- per issue, found again by the alert's fingerprint
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_kind_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_kind_check CHECK (kind IN ('gate_bound', 'supervisor_trip', 'escalation', 'info_request', 'alert'));
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS fingerprint text;
CREATE INDEX IF NOT EXISTS alerts_fingerprint_idx ON alerts (fingerprint, id) WHERE fingerprint IS NOT NULL;
