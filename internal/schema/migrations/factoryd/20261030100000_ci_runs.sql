-- ci_runs (roadmap 147): one row per finished Actions run of a
-- repository, from the push that created it to its last job's finish.
-- Append-only and unique on (repository, commit), so the first run of a
-- commit carries the push time lead time and the CI duration read.
CREATE TABLE IF NOT EXISTS ci_runs (
    repository text NOT NULL,
    commit text NOT NULL,
    pushed_at timestamptz NOT NULL,
    duration_ms bigint NOT NULL,
    PRIMARY KEY (repository, commit)
);
