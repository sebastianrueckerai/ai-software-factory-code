-- the question's shape (the question-format concept, issue
-- ISSUE-96679639121084ea): an escalation row carries the five-part
-- question the card asked -- ask, why, info, recommendation with its
-- source, and the choices with their values -- so the console renders the
-- ask as the headline and the rest under it, with nothing recomposed at
-- read. Rows before the column keep NULL and read as today: reason text,
-- bare choices.
ALTER TABLE intake_events ADD COLUMN IF NOT EXISTS question JSONB;
