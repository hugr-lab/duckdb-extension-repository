-- +min_reader 14
-- spec 0016 phase 2: the slots of purged releases stay taken; an older replica would refill them
CREATE TABLE purged_slots (
	channel_id  varchar(36) NOT NULL,
	name        varchar(64) NOT NULL,
	ext_version varchar(64) NOT NULL,
	platform    varchar(64) NOT NULL,
	slot        varchar(72) NOT NULL,
	abi         varchar(24) NOT NULL,
	body_hash   varchar(64) NOT NULL,
	seq         bigint,
	release_id  varchar(36) NOT NULL,
	purged_at   timestamptz NOT NULL,
	PRIMARY KEY (channel_id, name, ext_version, platform, slot)
)
