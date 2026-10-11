-- project_mirrors: a project's mirror on GitHub, one per direction. An
-- export row is a Forgejo push mirror of the project's repository, its
-- token written once for Forgejo and read back by the registrar alone,
-- its state the registrar's reading of the push mirror's last sync.
CREATE TABLE IF NOT EXISTS project_mirrors (
	project text NOT NULL,
	direction text NOT NULL CHECK (direction IN ('export', 'import')),
	remote text NOT NULL,
	username text NOT NULL,
	token text NOT NULL CHECK (token <> ''),
	state text NOT NULL DEFAULT '',
	last_error text NOT NULL DEFAULT '',
	last_update timestamptz,
	updated_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (project, direction)
);
