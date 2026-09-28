-- who made the call: an agent, factoryd for a purpose (review, supervisor,
-- gate-supervisor) on the task's account, or factoryd refused before its
-- purpose was known (factoryd)
ALTER TABLE audit ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'agent';
