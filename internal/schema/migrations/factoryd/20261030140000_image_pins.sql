-- the sandbox image pins the imager last saw in force, one row per project
-- image: key and digest the pin, previous_key and previous_digest the pin
-- before it ('' for none seen), since when the name has been pinned,
-- retained whether the store holds no copy of the image but the two pins',
-- removal_task the last card filed to remove the image as unused ('' for
-- none)
CREATE TABLE IF NOT EXISTS image_pins (
	project text NOT NULL,
	image text NOT NULL,
	key text NOT NULL,
	digest text NOT NULL,
	previous_key text NOT NULL DEFAULT '',
	previous_digest text NOT NULL DEFAULT '',
	since timestamptz NOT NULL,
	retained boolean NOT NULL DEFAULT false,
	removal_task text NOT NULL DEFAULT '',
	PRIMARY KEY (project, image)
);
