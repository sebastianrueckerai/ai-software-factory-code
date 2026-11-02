-- A plan's short name, set by the naming pass once the plan is filed:
-- the model's answer, or the deterministic slug when it fails or
-- refuses. The name is cosmetic: uniqueness among the project's open
-- plans lives in the naming pass, not the schema.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS plan_name text;
