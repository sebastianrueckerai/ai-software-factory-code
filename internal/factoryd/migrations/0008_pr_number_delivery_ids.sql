ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pr_number bigint NOT NULL DEFAULT 0;
ALTER TABLE intake_events ADD COLUMN IF NOT EXISTS delivery_id text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS intake_events_delivery_id_key ON intake_events (delivery_id) WHERE delivery_id <> '';
