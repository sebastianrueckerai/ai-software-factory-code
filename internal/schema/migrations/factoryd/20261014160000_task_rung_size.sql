-- tasks gain their rung and size (roadmap 222, item 5): rung is the
-- model rung the card starts on, cheap or frontier, chosen at filing
-- from the issue's "Rung:" line or derived from the size, the kind and
-- the directories; size is the "Size:" line, kept so stronger_after can
-- follow it. Both empty where nothing was asked.
ALTER TABLE tasks ADD COLUMN rung text NOT NULL DEFAULT '', ADD COLUMN size text NOT NULL DEFAULT '';
