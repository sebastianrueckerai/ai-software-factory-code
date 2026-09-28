-- one row per roll of main (factoryd records its own at boot): the push of
-- the commit, Flux applying it, both Deployments available on it
CREATE TABLE IF NOT EXISTS rolls (
    sha        text PRIMARY KEY,
    pushed_at  timestamptz NOT NULL,
    applied_at timestamptz NOT NULL,
    live_at    timestamptz NOT NULL
);
