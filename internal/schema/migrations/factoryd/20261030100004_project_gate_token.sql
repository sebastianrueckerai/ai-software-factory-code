-- projects.gate_token (ruling R59): factory-gate's write:repository
-- limited to the repository, minted by registration like merge_token; the
-- merge gate's approval of a merge on the merge rule auto
ALTER TABLE projects ADD COLUMN IF NOT EXISTS gate_token text NOT NULL DEFAULT '';
