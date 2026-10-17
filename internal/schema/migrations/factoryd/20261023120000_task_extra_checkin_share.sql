-- the check-in budget's share raises: one per answer a human gave at the
-- share stop (issue 959), so the stretch the answer buys runs its own
-- check-in moments while the card's own budget stays the cap
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS extra_checkin_share integer NOT NULL DEFAULT 0;
