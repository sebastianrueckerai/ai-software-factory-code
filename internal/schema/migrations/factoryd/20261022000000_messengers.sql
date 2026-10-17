-- the messengers (roadmap 145): the chats a member connected on Telegram
-- and Discord, the one-time codes that bind them, the member's notification
-- level per project, and the outbox row per notification a messenger owes.
CREATE TABLE IF NOT EXISTS messenger_links (
	id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	login text NOT NULL,
	venue text NOT NULL CHECK (venue IN ('telegram', 'discord')),
	address text NOT NULL, -- the Telegram chat or the Discord DM channel
	venue_user text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (login, venue), -- one link per member and venue
	UNIQUE (venue, address) -- a chat belongs to one member
);

CREATE TABLE IF NOT EXISTS messenger_codes (
	code_hash text PRIMARY KEY, -- SHA-256 hex of the one-time code
	login text NOT NULL,
	expires_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS notify_levels (
	login text NOT NULL,
	repository text NOT NULL,
	level text NOT NULL CHECK (level IN ('off', 'needs-you', 'all')),
	PRIMARY KEY (login, repository) -- no row: the derived default
);

CREATE TABLE IF NOT EXISTS notify_deliveries (
	id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	message_id bigint NOT NULL, -- the inbox row the notification copies
	link_id bigint NOT NULL REFERENCES messenger_links(id) ON DELETE CASCADE,
	state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sent', 'failed')),
	tries int NOT NULL DEFAULT 0,
	next_try timestamptz NOT NULL DEFAULT now(),
	ref text NOT NULL DEFAULT '', -- the sent message's id on the venue
	sent_at timestamptz,
	edited_at timestamptz, -- the settle edit landed
	last_error text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS notify_deliveries_due_idx ON notify_deliveries (state, next_try);
CREATE INDEX IF NOT EXISTS notify_deliveries_message_idx ON notify_deliveries (message_id);
