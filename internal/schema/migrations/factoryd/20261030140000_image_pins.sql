-- the sandbox image pins the imager last saw in force, one row per project
-- image: key and digest the pin, previous_key and previous_digest the pin
-- before it ('' for none seen), since when the name has been pinned,
-- unpinned_at when the imager first saw it no longer pinned (null while it
-- is), retained whether the store holds no build of the image but those it
-- keeps, removal_task the last card filed to remove the image as unused (''
-- for none)
CREATE TABLE IF NOT EXISTS image_pins (
	project text NOT NULL,
	image text NOT NULL,
	key text NOT NULL,
	digest text NOT NULL,
	previous_key text NOT NULL DEFAULT '',
	previous_digest text NOT NULL DEFAULT '',
	since timestamptz NOT NULL,
	unpinned_at timestamptz,
	retained boolean NOT NULL DEFAULT false,
	removal_task text NOT NULL DEFAULT '',
	PRIMARY KEY (project, image)
);
