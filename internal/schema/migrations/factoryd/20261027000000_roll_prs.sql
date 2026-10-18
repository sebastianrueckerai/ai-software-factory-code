-- the pull requests a roll request carries (roadmap 296): the link a card
-- reads to track its own deploy, the roll of the commit its merge made
ALTER TABLE roll_requests ADD COLUMN IF NOT EXISTS prs bigint[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS roll_requests_prs_idx ON roll_requests USING gin (prs);
