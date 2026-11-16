-- A card stores its brief in fields: beside the title, the opening
-- paragraph of its objective, its why line and its summary, and the mark of
-- a brief filed after its writer's last refused try.
ALTER TABLE tasks
  ADD COLUMN IF NOT EXISTS why text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS summary text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS brief_unchecked boolean NOT NULL DEFAULT false;

-- A human's summary, which replaced the title on the board alone, moves
-- into the summary, so no text a human wrote is lost, and its column goes.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema() AND table_name = 'tasks' AND column_name = 'human_summary') THEN
    UPDATE tasks SET summary = human_summary WHERE COALESCE(human_summary, '') <> '';
    ALTER TABLE tasks DROP COLUMN human_summary;
  END IF;
END $$;
