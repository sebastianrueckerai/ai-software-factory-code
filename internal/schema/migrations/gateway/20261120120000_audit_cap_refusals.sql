-- the console reads whether a cap refused a call in its current period
-- (policy.LedgerEnforced, factoryd's ProjectMonthSpend) on every board read
CREATE INDEX IF NOT EXISTS audit_cap_refusals_idx ON audit (ts)
    WHERE decision IN ('refused_day_cap', 'refused_month_cap', 'refused_premium_cap', 'refused_project_cap');
