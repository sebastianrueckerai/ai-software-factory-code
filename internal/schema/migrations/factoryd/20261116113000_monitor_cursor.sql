-- The monitor's cursor: the last event whose card the monitor read. Its
-- first pass starts at the newest event, so no history raises a flag.
CREATE TABLE monitor_cursor (one boolean PRIMARY KEY DEFAULT true CHECK (one), event_id bigint NOT NULL);
