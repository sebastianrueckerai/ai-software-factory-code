-- The pod a live session runs in, the node it sits on and that node's boot.
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS pod_uid text NOT NULL DEFAULT '';
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS node text NOT NULL DEFAULT '';
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS boot_id text NOT NULL DEFAULT '';

-- What the platform did to the cluster, once per key: a kill, a restart, a
-- node boot, a node that was not ready.
CREATE TABLE IF NOT EXISTS platform_incidents (
	id bigserial PRIMARY KEY,
	key text NOT NULL UNIQUE,
	at timestamptz NOT NULL,
	until timestamptz,
	kind text NOT NULL CHECK (kind IN ('guard_kill', 'kernel_oom', 'restart', 'node_boot', 'node_not_ready')),
	subject text NOT NULL,
	node text NOT NULL DEFAULT '',
	namespace text NOT NULL DEFAULT '',
	pod_uid text NOT NULL DEFAULT '',
	detail jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS platform_incidents_at ON platform_incidents (at);
CREATE INDEX IF NOT EXISTS platform_incidents_pod_uid ON platform_incidents (pod_uid) WHERE pod_uid <> '';

-- Who ended a pod, one row per session, CI runner or gate pod.
CREATE TABLE IF NOT EXISTS pod_deaths (
	key text PRIMARY KEY,
	kind text NOT NULL CHECK (kind IN ('session', 'ci', 'gate')),
	task_id text REFERENCES tasks(id),
	session_id text NOT NULL DEFAULT '',
	runner text NOT NULL DEFAULT '',
	at timestamptz NOT NULL,
	class text NOT NULL CHECK (class IN ('platform', 'own_limit', 'unknown')),
	record jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS pod_deaths_platform_at ON pod_deaths (at) WHERE class = 'platform';
