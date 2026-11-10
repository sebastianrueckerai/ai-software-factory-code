-- The RunSession attempt that runs the session: a stale attempt finds a
-- newer owner and leaves the pod and the credentials alone.
ALTER TABLE live_sessions ADD COLUMN owner text NOT NULL DEFAULT '';
