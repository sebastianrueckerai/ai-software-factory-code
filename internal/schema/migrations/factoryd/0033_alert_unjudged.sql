-- an escalation or info_request alert whose task ended before an approver
-- judged it
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_verdict_check;
ALTER TABLE alerts ADD CONSTRAINT alerts_verdict_check CHECK (verdict IN ('real', 'noise', 'unjudged'));
