-- when a factory project's board became ready: set by the first reconcile
-- whose every object is in place (stage11-design.md section 5), never
-- cleared
ALTER TABLE projects ADD COLUMN IF NOT EXISTS ready_at timestamptz;
