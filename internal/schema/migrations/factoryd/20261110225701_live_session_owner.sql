-- The RunSession attempt that runs the session, and the activity it
-- belongs to: a stale attempt finds a newer owner and leaves the pod and
-- the credentials alone, and a later session's activity never reads a
-- pod's result that was not its own.
ALTER TABLE live_sessions ADD COLUMN owner text NOT NULL DEFAULT '';
ALTER TABLE live_sessions ADD COLUMN activity text NOT NULL DEFAULT '';
