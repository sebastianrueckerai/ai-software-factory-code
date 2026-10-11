-- The answers an agent offers with its question (escalate_to_human's
-- choices), on the intake events that hold the question: the agent's ask
-- and the escalation the board reads. NULL when it offered none.
ALTER TABLE intake_events ADD COLUMN IF NOT EXISTS choices jsonb;
