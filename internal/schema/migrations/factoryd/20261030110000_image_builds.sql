-- the imager (sandbox images): per project image and build key, when
-- factoryd last dispatched its build and how often it did, and when a pass
-- last read it as the image's desired key
CREATE TABLE IF NOT EXISTS image_builds (
	project text NOT NULL,
	image text NOT NULL,
	key text NOT NULL,
	dispatched_at timestamptz,
	dispatches integer NOT NULL DEFAULT 0,
	read_at timestamptz,
	PRIMARY KEY (project, image, key)
);
-- the imager's alerts: one row per cause, keyed by fingerprint
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_kind_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_kind_check CHECK (kind IN ('gate_bound', 'supervisor_trip', 'escalation', 'info_request', 'alert', 'config_rejected', 'screening', 'month_cap', 'chain_red', 'chain_stalled', 'chain_uncarried', 'image'));
CREATE UNIQUE INDEX IF NOT EXISTS alerts_image_key ON alerts (kind, fingerprint) WHERE kind = 'image';
