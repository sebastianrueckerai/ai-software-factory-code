-- the booking's COST OVER HOLD flag, on the audit row of whatever decision
-- the call got, and the decision allowed_over_hold folded into it
ALTER TABLE audit ADD COLUMN IF NOT EXISTS over_hold text NOT NULL DEFAULT '';
UPDATE audit SET over_hold = reason,
    decision = CASE WHEN tool = 'commission_server' THEN 'accrued' ELSE 'allowed' END
WHERE decision = 'allowed_over_hold';
