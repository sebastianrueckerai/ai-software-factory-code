-- who promoted or released an agent resource in the console (task 54)
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS promoted_by text NOT NULL DEFAULT '';
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS released_by text NOT NULL DEFAULT '';
