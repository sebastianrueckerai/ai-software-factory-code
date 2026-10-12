-- the GitHub default branch's head the export's last Sync latest brought
-- back, and what that sync answered
ALTER TABLE project_mirrors ADD COLUMN IF NOT EXISTS synced text NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS sync_result text NOT NULL DEFAULT '';
