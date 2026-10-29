-- +min_reader 45
-- spec 0036 phase 2: a download count and a day's installers record whose entry they were, the
-- release's source (upstream:<id>, publisher:<id>, tenant), in their keys, so a distributor's
-- downloads can be summed over tenants, into weekly snapshots of its own (distributor_weeks). Rows
-- before it get the source '' and count for no distributor. An older replica would add counts by
-- the old key, which the new one no longer matches, so it no longer runs here.
-- Migration 0011 left the primary keys system-named: each is found by its table and dropped.
ALTER TABLE download_counts ADD source nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL
	CONSTRAINT df_download_counts_source DEFAULT ''
-- +statement
DECLARE @n sysname = (SELECT name FROM sys.key_constraints WHERE parent_object_id = OBJECT_ID('download_counts') AND type = 'PK');
IF @n IS NULL THROW 50000, 'migration 0045: the download_counts primary key was not found', 1;
DECLARE @q nvarchar(300) = N'ALTER TABLE download_counts DROP CONSTRAINT ' + QUOTENAME(@n);
EXEC (@q)
-- +statement
ALTER TABLE download_counts ADD CONSTRAINT pk_download_counts
PRIMARY KEY (tenant_id, channel_id, source, name, package_version, platform, duckdb_version, day, authenticated)
-- +statement
CREATE INDEX ix_download_counts_source ON download_counts (source, day)
-- +statement
ALTER TABLE download_installers ADD source nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL
	CONSTRAINT df_download_installers_source DEFAULT ''
-- +statement
DECLARE @n sysname = (SELECT name FROM sys.key_constraints WHERE parent_object_id = OBJECT_ID('download_installers') AND type = 'PK');
IF @n IS NULL THROW 50000, 'migration 0045: the download_installers primary key was not found', 1;
DECLARE @q nvarchar(300) = N'ALTER TABLE download_installers DROP CONSTRAINT ' + QUOTENAME(@n);
EXEC (@q)
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
	distributor      nvarchar(128) COLLATE Latin1_General_100_BIN2 NOT NULL,
	package_type     nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	week             nvarchar(10) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name             nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	downloads        bigint NOT NULL,
	public_downloads bigint NOT NULL,
	installer_days   bigint NULL,
	withheld         bit NOT NULL,
	PRIMARY KEY (distributor, package_type, week, name)
)
-- +statement
CREATE TABLE distributor_weeks_done (
	week        nvarchar(10) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	min_tenants int NOT NULL,
	done_at     datetime2(6) NOT NULL
)
