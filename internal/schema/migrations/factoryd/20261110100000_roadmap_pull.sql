-- The roadmap pull (2026-11-01): one row per repository holds the head
-- of docs/roadmap.md its last pull turn read, so the same roadmap never
-- wakes the project twice, and tasks carry the roadmap item a pulled
-- card builds, which no second card of the repository may cite.

CREATE TABLE IF NOT EXISTS roadmap_pull (
	repository text PRIMARY KEY,
	head text NOT NULL,
	at timestamptz NOT NULL
);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS roadmap_item integer;
