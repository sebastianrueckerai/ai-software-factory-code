-- One fact, one row, whichever retry records it first: the fact
-- payloads of internal/factoryd/metrics.go set Event.IdentityKey — a
-- plan filing's plan and revision (plan_replanned), a hold's repository
-- and held-since instant (production_released) — and the partial index
-- holds one row per key; insertEvent answers a repeat with ON CONFLICT
-- DO NOTHING, so a retry leaves one row and one id.
-- No row the factory already holds is touched: the column starts null
-- and the index covers only rows that carry a key, so every historical
-- event keeps its evidence, duplicate retries among them. Keys are
-- written only by the recorders from here on: prospective, no
-- backfill to guess (coverage, not repair).
ALTER TABLE events ADD COLUMN IF NOT EXISTS identity_key text;
CREATE UNIQUE INDEX IF NOT EXISTS events_identity_once_idx ON events (identity_key) WHERE identity_key IS NOT NULL;
