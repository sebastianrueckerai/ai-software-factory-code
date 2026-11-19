-- A card stores its brief in fields: beside the title, the opening
-- paragraph of its objective, its why line and its summary, the mark of a
-- brief filed after its writer's last refused try, and whether the project
-- agent wrote the card in a human's chat, which the human authors.
ALTER TABLE tasks
  ADD COLUMN IF NOT EXISTS why text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS summary text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS brief_unchecked boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS chat_filed boolean NOT NULL DEFAULT false;

-- A human's summary, which replaced the title on the board alone, moves
-- into the summary, so no text a human wrote is lost. The column stays for
-- one release: the factoryd before this one reads it while the roll runs
-- and after a rollback. The next release drops it.
UPDATE tasks SET summary = human_summary WHERE COALESCE(human_summary, '') <> '' AND summary = '';
