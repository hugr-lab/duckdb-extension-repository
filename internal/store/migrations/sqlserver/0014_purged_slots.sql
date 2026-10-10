-- +min_reader 14
-- spec 0016 phase 2: the slots of purged releases stay taken; an older replica would refill them
CREATE TABLE purged_slots (
	channel_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name        nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ext_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	platform    nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	slot        nvarchar(72) COLLATE Latin1_General_100_BIN2 NOT NULL,
	abi         nvarchar(24) COLLATE Latin1_General_100_BIN2 NOT NULL,
	body_hash   nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	seq         bigint NULL,
	release_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	purged_at   datetime2(6) NOT NULL,
	PRIMARY KEY (channel_id, name, ext_version, platform, slot)
)
