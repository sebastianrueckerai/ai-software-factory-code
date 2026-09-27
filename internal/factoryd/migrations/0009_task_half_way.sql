ALTER TABLE tasks ADD COLUMN IF NOT EXISTS half_way_done boolean NOT NULL DEFAULT false;
