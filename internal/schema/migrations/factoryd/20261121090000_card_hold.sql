-- A human's hold on a card that has not started: it gets no work slot
-- until a human releases it or its release card, any card of the same
-- repository, ends done or merged. A release card that ends failed or
-- closed keeps the hold.
ALTER TABLE tasks
  ADD COLUMN IF NOT EXISTS hold_at timestamptz,
  ADD COLUMN IF NOT EXISTS hold_by text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS hold_reason text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS hold_release text REFERENCES tasks (id);
CREATE INDEX IF NOT EXISTS tasks_hold_release ON tasks (hold_release) WHERE hold_release IS NOT NULL;
