-- +min_reader 33
-- spec 0032: lazy mirrors. A mirror fetches, when lazy, only the cells someone asked for and its
-- eager entries; an older replica would run a lazy upstream as an eager one, so it no longer runs
-- here.
ALTER TABLE upstreams ADD COLUMN fetch_mode varchar(8) NOT NULL DEFAULT 'eager'
-- +statement
ALTER TABLE upstream_entries ADD COLUMN eager boolean NOT NULL DEFAULT false
-- +statement
CREATE TABLE upstream_asks (
	upstream_id    varchar(36) NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	duckdb_version varchar(64) NOT NULL,
	platform       varchar(64) NOT NULL,
	name           varchar(64) NOT NULL,
	asked_at       timestamptz NOT NULL,
	PRIMARY KEY (upstream_id, duckdb_version, platform, name)
)
