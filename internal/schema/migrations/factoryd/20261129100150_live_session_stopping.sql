-- When factoryd sent its stop to the session's pod: a pod gone or deleting
-- after that is factoryd's own stop, also for the next factoryd that
-- reattaches.
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS stopping_at timestamptz
