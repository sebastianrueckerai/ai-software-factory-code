-- the check-in shares a human's answers to the check-in's spend stop
-- granted on top of the policy's (issue 959): the card's check-in share is
-- CheckinShareOf its budget times 1+extra_checkin, so the stretch an answer
-- buys can run its own check-in moments; the card's own budget stays the cap
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS extra_checkin integer NOT NULL DEFAULT 0;
