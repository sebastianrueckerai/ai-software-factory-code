-- a human's reset of a cap's count: the day's, the month's or a project's
-- cap counts from its newest reset in its current UTC period, and before_eur
-- is the count the reset ended, holds included
CREATE TABLE IF NOT EXISTS cap_resets (
    id bigserial PRIMARY KEY,
    cap text NOT NULL CHECK (cap IN ('day', 'month', 'project')),
    repository text NOT NULL DEFAULT '',
    at timestamptz NOT NULL DEFAULT now(),
    login text NOT NULL,
    before_eur numeric NOT NULL,
    CHECK ((cap = 'project') = (repository <> ''))
);
CREATE INDEX IF NOT EXISTS cap_resets_newest_idx ON cap_resets (cap, repository, at DESC);
