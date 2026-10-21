-- projects.chat_token: factory-chat's write:repository and write:issue
-- limited to the repository, minted by registration like gate_token; the
-- project agent's own identity in every turn no member started
ALTER TABLE projects ADD COLUMN IF NOT EXISTS chat_token text NOT NULL DEFAULT '';
