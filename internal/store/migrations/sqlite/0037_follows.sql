-- +min_reader 37
-- spec 0033 phase A1 part 4: a channel follows a whole source: every name of it the channel has no
-- rule for is taken automatically, but a name the tenant uses from another source, a reserved one, or
-- one two follows offer; the names a follow skips are kept with the reason. An older replica would
-- treat a follow's rules as manual ones, so it no longer runs here.
CREATE TABLE channel_follows (
	channel_id TEXT NOT NULL REFERENCES channels (id),
	source     TEXT NOT NULL,
	set_by     TEXT NOT NULL,
	set_at     TEXT NOT NULL,
	checked_at TEXT,
	PRIMARY KEY (channel_id, source)
)
-- +statement
CREATE TABLE follow_skips (
	channel_id TEXT NOT NULL,
	source     TEXT NOT NULL,
	name       TEXT NOT NULL,
	reason     TEXT NOT NULL,
	PRIMARY KEY (channel_id, source, name)
)
-- +statement
-- the placement pass asks, per follow, for its source's builds made since its last look
CREATE INDEX ix_builds_source_created ON builds (tenant_id, source, created_at)
