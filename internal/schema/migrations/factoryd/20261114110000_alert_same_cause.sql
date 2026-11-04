-- a second escalation of one fault, linked to the first card in its
-- detail's same_as
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_verdict_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_verdict_check CHECK (verdict IN ('real', 'noise', 'unjudged', 'same_cause'));
