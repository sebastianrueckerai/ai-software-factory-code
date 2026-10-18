-- The factory's own dependency health (roadmap 299): one row per probed
-- dependency, its latest state only. The prober overwrites; there is no
-- history. The console's status panel lists the rows and its dot
-- summarises them.

CREATE TABLE IF NOT EXISTS dependency_health (
	name text PRIMARY KEY,
	state text NOT NULL CHECK (state IN ('up', 'degraded', 'down')),
	detail text NOT NULL DEFAULT '',
	latency_ms integer NOT NULL DEFAULT 0,
	checked_at timestamptz NOT NULL DEFAULT now()
);
