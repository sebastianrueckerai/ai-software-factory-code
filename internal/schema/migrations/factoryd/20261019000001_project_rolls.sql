-- project_rolls (the hard data of a project over time, roadmap 198): one
-- row per project deployment the CI rollout wait confirmed, ready_at the
-- confirmation its poll saw. The push time joins from the CI run of the
-- same commit that factoryd's CI metrics already read, so lead time needs
-- no collection of its own.
CREATE TABLE IF NOT EXISTS project_rolls (
	id bigserial PRIMARY KEY,
	repository text NOT NULL,
	namespace text NOT NULL,
	digest text NOT NULL,
	commit text NOT NULL DEFAULT '',
	ready_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (repository, digest)
);
