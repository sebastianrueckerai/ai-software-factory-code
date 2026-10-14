-- audit gains the chat thread a factoryd call served (roadmap 239): the
-- thread's newest call's tokens project its next turn's context, and its
-- rows sum to the thread's token and cost totals.
ALTER TABLE audit ADD COLUMN IF NOT EXISTS thread text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS audit_thread_idx ON audit (thread, ts DESC) WHERE thread <> '';
