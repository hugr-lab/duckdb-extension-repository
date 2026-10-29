-- +min_reader 45
-- spec 0036 phase 2: a download count and a day's installers record whose entry they were, the
-- release's source (upstream:<id>, publisher:<id>, tenant), in their keys, so a distributor's
-- downloads can be summed over tenants, into weekly snapshots of its own (distributor_weeks). Rows
-- before it get the source '' and count for no distributor. An older replica would add counts by
-- the old key, which the new one no longer matches, so it no longer runs here.
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
-- +statement
-- a distributor's weekly figures, computed once when the week is complete and never again: a
-- snapshot per (distributor, package type, week, name), and the weeks done (the server's,
-- of no tenant)
CREATE TABLE distributor_weeks (
	distributor      varchar(128) NOT NULL,
	package_type     varchar(32) NOT NULL,
	week             varchar(10) NOT NULL,
	name             varchar(64) NOT NULL,
	downloads        bigint NOT NULL,
	public_downloads bigint NOT NULL,
	installer_days   bigint NULL,
	withheld         boolean NOT NULL,
	PRIMARY KEY (distributor, package_type, week, name)
)
-- +statement
CREATE TABLE distributor_weeks_done (
	week        varchar(10) PRIMARY KEY,
	min_tenants integer NOT NULL,
	done_at     timestamptz NOT NULL
)
