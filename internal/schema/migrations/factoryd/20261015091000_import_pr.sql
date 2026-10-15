-- an import row's infra pull request, the one that files its project, 0
-- before it opens: the row lives until registration consumes it or that
-- pull request closes unmerged. A row filed before takes the pull request
-- of its project's last project_filed event that names the row's remote.
ALTER TABLE project_mirrors ADD COLUMN IF NOT EXISTS pr bigint NOT NULL DEFAULT 0;
UPDATE project_mirrors m SET pr = f.pr
	FROM (SELECT DISTINCT ON (detail->>'name') detail->>'name' AS project, detail->>'source' AS source, (detail->>'pr')::bigint AS pr
		FROM events WHERE kind = 'project_filed' ORDER BY detail->>'name', id DESC) f
	WHERE m.direction = 'import' AND m.pr = 0 AND f.project = m.project AND f.source = m.remote;
