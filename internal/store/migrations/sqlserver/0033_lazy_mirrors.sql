-- +min_reader 33
-- spec 0032: lazy mirrors. A mirror fetches, when lazy, only the cells someone asked for and its
-- eager entries; an older replica would run a lazy upstream as an eager one, so it no longer runs
-- here.
ALTER TABLE upstreams ADD fetch_mode nvarchar(8) COLLATE Latin1_General_100_BIN2 NOT NULL CONSTRAINT df_upstreams_fetch_mode DEFAULT N'eager'
-- +statement
ALTER TABLE upstream_entries ADD eager bit NOT NULL CONSTRAINT df_upstream_entries_eager DEFAULT 0
-- +statement
CREATE TABLE upstream_asks (
	upstream_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	duckdb_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	platform       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name           nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	asked_at       datetime2(6) NOT NULL,
	PRIMARY KEY (upstream_id, duckdb_version, platform, name)
)
