-- an export's mode: mirror, Forgejo's push mirror of every branch and tag,
-- or pull_requests, each card merged since since pushed to GitHub and
-- opened there as a pull request
ALTER TABLE project_mirrors ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'mirror' CHECK (mode IN ('mirror', 'pull_requests')),
	ADD COLUMN IF NOT EXISTS since timestamptz NOT NULL DEFAULT now();
-- a card's pull request on GitHub in an export of mode pull_requests: its
-- number and URL, and open, merged, closed or failed: <why>
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS github_pr bigint NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS github_pr_url text NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS github_pr_state text NOT NULL DEFAULT '';
