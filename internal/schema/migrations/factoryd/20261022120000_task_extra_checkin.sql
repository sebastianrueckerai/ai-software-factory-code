-- the check-in call cap's raises: one per answer a human gave at the
-- cap, so the card's next check-in runs instead of stopping before any
-- model call
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS extra_checkin integer NOT NULL DEFAULT 0;
