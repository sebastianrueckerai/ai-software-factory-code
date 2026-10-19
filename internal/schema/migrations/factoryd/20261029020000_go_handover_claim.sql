-- the go action's hand-over claim: one row per card, so of two racing
-- hands InsertIntakeEvent's ON CONFLICT DO NOTHING returns 0 to the loser
-- and only the first files.

CREATE UNIQUE INDEX IF NOT EXISTS intake_events_task_handover_key ON intake_events (task_id) WHERE kind = 'handover';
