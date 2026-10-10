-- +min_reader 14
-- spec 0016 phase 2: the slots of purged releases stay taken; an older replica would refill them
CREATE TABLE purged_slots (
	channel_id  TEXT NOT NULL,
	name        TEXT NOT NULL,
	ext_version TEXT NOT NULL,
	platform    TEXT NOT NULL,
	slot        TEXT NOT NULL,
	abi         TEXT NOT NULL,
	body_hash   TEXT NOT NULL,
	seq         INTEGER,
	release_id  TEXT NOT NULL,
	purged_at   TEXT NOT NULL,
	PRIMARY KEY (channel_id, name, ext_version, platform, slot)
)
