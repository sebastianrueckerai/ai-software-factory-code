-- One revision, one row: a plan_replanned event is unique per task and
-- the plan revision it names, whichever retry records it first. One
-- hold, one row: a production_released event is unique per repository
-- and the held-since instant it closed. A later duplicate of either
-- would stop the index, so the later row of each such pair goes first
-- and the earliest stays. insertEvent answers both with ON CONFLICT DO
-- NOTHING, so a retry lands one row and one id.
DELETE FROM events later USING events earlier
WHERE later.kind = 'plan_replanned' AND earlier.kind = 'plan_replanned'
  AND later.task_id = earlier.task_id AND later.detail->>'revision' = earlier.detail->>'revision'
  AND later.id > earlier.id;
CREATE UNIQUE INDEX IF NOT EXISTS events_plan_revision_once_idx ON events (task_id, (detail->>'revision'))
  WHERE kind = 'plan_replanned' AND detail->>'revision' IS NOT NULL;
DELETE FROM events later USING events earlier
WHERE later.kind = 'production_released' AND earlier.kind = 'production_released'
  AND later.repository = earlier.repository AND later.detail->>'held_since' = earlier.detail->>'held_since'
  AND later.id > earlier.id;
CREATE UNIQUE INDEX IF NOT EXISTS events_release_once_idx ON events (repository, (detail->>'held_since'))
  WHERE kind = 'production_released' AND detail->>'held_since' IS NOT NULL;
