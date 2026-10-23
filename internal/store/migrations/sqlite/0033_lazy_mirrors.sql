-- +min_reader 33
-- spec 0032: lazy mirrors. A mirror fetches, when lazy, only the cells someone asked for and its
-- eager entries; an older replica would run a lazy upstream as an eager one, so it no longer runs
-- here.
ALTER TABLE upstreams ADD COLUMN fetch_mode TEXT NOT NULL DEFAULT 'eager'
-- +statement
ALTER TABLE upstream_entries ADD COLUMN eager INTEGER NOT NULL DEFAULT 0
-- +statement
CREATE TABLE upstream_asks (
	upstream_id    TEXT NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	duckdb_version TEXT NOT NULL,
	platform       TEXT NOT NULL,
	name           TEXT NOT NULL,
	asked_at       TEXT NOT NULL,
	PRIMARY KEY (upstream_id, duckdb_version, platform, name)
)
