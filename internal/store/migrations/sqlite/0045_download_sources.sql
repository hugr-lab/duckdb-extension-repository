-- +min_reader 45
-- spec 0036 phase 2: a download count and a day's installers record whose entry they were, the
-- release's source (upstream:<id>, publisher:<id>, tenant), in their keys, so a distributor's
-- downloads can be summed over tenants. Rows before it get the source '' and count for no
-- distributor. An older replica would add counts by the old key, which the new one no longer
-- matches, so it no longer runs here. SQLite rebuilds both tables; no other table references them,
-- so foreign keys stay on, and the indexes are made after the old tables are gone.
CREATE TABLE download_counts_new (
	tenant_id       TEXT NOT NULL,
	channel_id      TEXT NOT NULL,
	source          TEXT NOT NULL DEFAULT '',
	name            TEXT NOT NULL,
	package_version TEXT NOT NULL,
	platform        TEXT NOT NULL,
	duckdb_version  TEXT NOT NULL,
	day             TEXT NOT NULL,
	authenticated   INTEGER NOT NULL,
	count           INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, source, name, package_version, platform, duckdb_version, day, authenticated)
)
-- +statement
INSERT INTO download_counts_new (tenant_id, channel_id, source, name, package_version, platform, duckdb_version, day, authenticated, count)
SELECT tenant_id, channel_id, '', name, package_version, platform, duckdb_version, day, authenticated, count FROM download_counts
-- +statement
DROP TABLE download_counts
-- +statement
ALTER TABLE download_counts_new RENAME TO download_counts
-- +statement
CREATE INDEX ix_download_counts_day ON download_counts (tenant_id, day)
-- +statement
CREATE INDEX ix_download_counts_source ON download_counts (source, day)
-- +statement
CREATE TABLE download_installers_new (
	tenant_id       TEXT NOT NULL,
	channel_id      TEXT NOT NULL,
	source          TEXT NOT NULL DEFAULT '',
	name            TEXT NOT NULL,
	package_version TEXT NOT NULL,
	platform        TEXT NOT NULL,
	day             TEXT NOT NULL,
	installers      INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, source, name, package_version, platform, day)
)
-- +statement
INSERT INTO download_installers_new (tenant_id, channel_id, source, name, package_version, platform, day, installers)
SELECT tenant_id, channel_id, '', name, package_version, platform, day, installers FROM download_installers
-- +statement
DROP TABLE download_installers
-- +statement
ALTER TABLE download_installers_new RENAME TO download_installers
-- +statement
CREATE INDEX ix_download_installers_day ON download_installers (tenant_id, day)
-- +statement
CREATE INDEX ix_download_installers_source ON download_installers (source, day)
