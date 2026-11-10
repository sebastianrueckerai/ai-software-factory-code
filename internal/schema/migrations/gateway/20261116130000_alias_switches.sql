-- each time the gateway moves an alias to its twin for being slow, and each
-- time it returns, with the seconds per 1000 output tokens (the window's sum
-- of duration over its sum of output tokens) that caused it; an alias whose newest row is a switch is open
CREATE TABLE IF NOT EXISTS alias_switches (
    id bigserial PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    alias text NOT NULL,
    twin text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('switch', 'return')),
    rate_s_per_1000 double precision NOT NULL,
    deployment text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS alias_switches_alias_idx ON alias_switches (alias, id DESC);
-- the gateway's latency window reads an alias's successful calls of the last hour
CREATE INDEX IF NOT EXISTS audit_alias_ts_idx ON audit (model_alias, ts) WHERE tool = '' AND decision = 'allowed';
