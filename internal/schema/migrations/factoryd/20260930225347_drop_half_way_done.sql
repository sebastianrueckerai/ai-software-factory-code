-- half_way_done (Stage 12, ruling 216): the half-way check-in gave way to
-- the check-in at every check_in interval, which needs no stored flag.
ALTER TABLE tasks DROP COLUMN IF EXISTS half_way_done;
