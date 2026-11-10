-- The card's enforced spend when the session started, so a reattached
-- session weighs its spend from its real start; null where unread.
ALTER TABLE live_sessions ADD COLUMN spent_at_start double precision;
