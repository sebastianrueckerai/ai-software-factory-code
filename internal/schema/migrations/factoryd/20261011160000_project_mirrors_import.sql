-- an import row holds no token for a public GitHub repository, nor after
-- registration's migrate used it
ALTER TABLE project_mirrors DROP CONSTRAINT IF EXISTS project_mirrors_token_check;
ALTER TABLE project_mirrors ADD CONSTRAINT project_mirrors_token_check CHECK (token <> '' OR direction = 'import');
