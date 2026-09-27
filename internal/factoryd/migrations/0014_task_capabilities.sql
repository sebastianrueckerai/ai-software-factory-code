CREATE TABLE IF NOT EXISTS backfills (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());

WITH once AS (INSERT INTO backfills (name) VALUES ('0014_task_capabilities') ON CONFLICT DO NOTHING RETURNING name)
UPDATE tasks SET required_capabilities = '["coder"]'
WHERE kind = 'agent' AND required_capabilities IN ('null'::jsonb, '[]'::jsonb) AND EXISTS (SELECT 1 FROM once);
