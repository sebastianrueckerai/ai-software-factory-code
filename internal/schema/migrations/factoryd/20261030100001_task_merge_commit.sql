-- tasks.merge_commit (roadmap 147): the commit the merge queue's merge
-- made on the base, written once; the first write stands.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS merge_commit text NOT NULL DEFAULT '';
