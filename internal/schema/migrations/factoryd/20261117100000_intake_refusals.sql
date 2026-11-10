-- Why the intake made no card for an issue, once per reason: the comment
-- on the issue, and the answer the project agent's card tool gives.
CREATE TABLE IF NOT EXISTS intake_refusals (
	repository text NOT NULL,
	issue_number bigint NOT NULL,
	key text NOT NULL,
	reason text NOT NULL,
	body_sha text NOT NULL,
	at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (repository, issue_number, key)
);

-- A card a refile took its roadmap item over from keeps its roadmap_item
-- for its history and holds the item no longer.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS roadmap_released boolean NOT NULL DEFAULT false;
