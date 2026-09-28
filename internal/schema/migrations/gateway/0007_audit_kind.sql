-- who made the call: an agent, or factoryd for a purpose (review,
-- supervisor, gate-supervisor) on the task's account
ALTER TABLE audit ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'agent';
