-- the agent time a plan card's one automatic planner session got when its
-- own ran short: non-zero once granted, so a card gets it once
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS retry_time_ns bigint NOT NULL DEFAULT 0;
