-- a project's settings are its file projects/<name>.yaml in the
-- configuration tree (ruling 179): the row keeps what registration minted
-- and nothing a file says, and a project's envelope is its file's too
ALTER TABLE projects DROP COLUMN IF EXISTS repository;
ALTER TABLE projects DROP COLUMN IF EXISTS merge_rule;
ALTER TABLE projects DROP COLUMN IF EXISTS notify;
ALTER TABLE projects DROP COLUMN IF EXISTS namespaces;
ALTER TABLE projects DROP COLUMN IF EXISTS environments;
ALTER TABLE projects DROP COLUMN IF EXISTS quota_cpu;
ALTER TABLE projects DROP COLUMN IF EXISTS quota_memory;
ALTER TABLE projects DROP COLUMN IF EXISTS quota_storage;
ALTER TABLE projects DROP COLUMN IF EXISTS secrets;
ALTER TABLE projects DROP COLUMN IF EXISTS managed;
DROP TABLE IF EXISTS envelopes;
