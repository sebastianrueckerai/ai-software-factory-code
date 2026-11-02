-- The console's Extend raises a card's caps in columns the workflow never
-- writes: raised_sessions beside extra_attempts, which the workflow's own
-- grants count, and extra_lines, which only the console raised already.
-- The workflow's usage snapshot overwrote both with the stale numbers it
-- held, so a raise on a running card was lost at its next write.
ALTER TABLE tasks ADD COLUMN raised_sessions integer NOT NULL DEFAULT 0;
