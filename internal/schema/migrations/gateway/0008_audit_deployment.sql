-- the LiteLLM deployment that answered a model call (its provider and
-- model, e.g. zai/glm-5.3): a tier alias spreads calls over several
ALTER TABLE audit ADD COLUMN IF NOT EXISTS deployment text NOT NULL DEFAULT '';
