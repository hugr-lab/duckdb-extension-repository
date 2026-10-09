-- spec 0010 phase 2: download counts (no principal, no address) and daily installers
CREATE TABLE download_counts (
	tenant_id      nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name           nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ext_version    nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	platform       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	duckdb_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	day            nvarchar(10) COLLATE Latin1_General_100_BIN2 NOT NULL,
	authenticated  bit NOT NULL,
	count          bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, platform, duckdb_version, day, authenticated)
)
-- +statement
CREATE INDEX ix_download_counts_day ON download_counts (tenant_id, day)
-- +statement
CREATE TABLE download_installers (
	tenant_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name        nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ext_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	platform    nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	day         nvarchar(10) COLLATE Latin1_General_100_BIN2 NOT NULL,
	installers  bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, platform, day)
)
-- +statement
CREATE INDEX ix_download_installers_day ON download_installers (tenant_id, day)
-- +statement
CREATE TABLE statistics_days (
	day     nvarchar(10) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	done_at datetime2(6) NOT NULL
)
