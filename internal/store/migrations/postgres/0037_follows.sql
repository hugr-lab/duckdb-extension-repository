-- +min_reader 37
-- spec 0033 phase A1 part 4: a channel follows a whole source: every name of it the channel has no
-- rule for is taken automatically, but a name the tenant uses from another source, a reserved one, or
-- one two follows offer; the names a follow skips are kept with the reason. An older replica would
-- treat a follow's rules as manual ones, so it no longer runs here.
CREATE TABLE channel_follows (
	channel_id varchar(36) NOT NULL REFERENCES channels (id),
	source     varchar(64) NOT NULL,
	set_by     varchar(400) NOT NULL,
	set_at     timestamptz NOT NULL,
	checked_at timestamptz,
	PRIMARY KEY (channel_id, source)
)
-- +statement
CREATE TABLE follow_skips (
	channel_id varchar(36) NOT NULL,
	source     varchar(64) NOT NULL,
	name       varchar(64) NOT NULL,
	reason     varchar(200) NOT NULL,
	PRIMARY KEY (channel_id, source, name)
)
