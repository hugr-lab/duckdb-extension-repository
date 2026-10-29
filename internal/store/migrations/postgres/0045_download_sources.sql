-- +min_reader 45
-- spec 0036 phase 2: a download count and a day's installers record whose entry they were, the
-- release's source (upstream:<id>, publisher:<id>, tenant), in their keys, so a distributor's
-- downloads can be summed over tenants. Rows before it get the source '' and count for no
-- distributor. An older replica would add counts by the old key, which the new one no longer
-- matches, so it no longer runs here.
ALTER TABLE download_counts ADD COLUMN source varchar(64) NOT NULL DEFAULT ''
-- +statement
ALTER TABLE download_counts DROP CONSTRAINT download_counts_pkey
-- +statement
ALTER TABLE download_counts ADD CONSTRAINT pk_download_counts
PRIMARY KEY (tenant_id, channel_id, source, name, package_version, platform, duckdb_version, day, authenticated)
-- +statement
CREATE INDEX ix_download_counts_source ON download_counts (source, day)
-- +statement
ALTER TABLE download_installers ADD COLUMN source varchar(64) NOT NULL DEFAULT ''
-- +statement
ALTER TABLE download_installers DROP CONSTRAINT download_installers_pkey
-- +statement
ALTER TABLE download_installers ADD CONSTRAINT pk_download_installers
PRIMARY KEY (tenant_id, channel_id, source, name, package_version, platform, day)
-- +statement
CREATE INDEX ix_download_installers_source ON download_installers (source, day)
