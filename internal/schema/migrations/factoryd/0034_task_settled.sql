-- when the spend of a task was set to the final figure of LiteLLM;
-- the ledger check that lets the settlement row of that go negative lives in
-- the gateway schema (internal/policy/schema.sql)
ALTER TABLE tasks ADD COLUMN settled_at timestamptz;
