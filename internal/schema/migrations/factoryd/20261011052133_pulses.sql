-- pulses (stage 15 phase 5): one row per computation of a project's pulse,
-- the sentence under its status line and the bullets behind it, prominent
-- when a card waits on a human. The newest row of a project is the one shown,
-- and each new row deletes its project's rows and pulse events older than a day.
CREATE TABLE IF NOT EXISTS pulses (
	id bigserial PRIMARY KEY,
	repository text NOT NULL,
	at timestamptz NOT NULL DEFAULT now(),
	sentence text NOT NULL,
	bullets jsonb NOT NULL DEFAULT '[]',
	prominent boolean NOT NULL DEFAULT false
);

CREATE INDEX IF NOT EXISTS pulses_newest_idx ON pulses (repository, id);
CREATE INDEX IF NOT EXISTS events_pulse_idx ON events (repository, at) WHERE kind IN ('pulse', 'pulse_computing');
