-- spec 0010 phase 2: download counts (no principal, no address) and daily installers
CREATE TABLE download_counts (
	tenant_id      varchar(36) NOT NULL,
	channel_id     varchar(36) NOT NULL,
	name           varchar(64) NOT NULL,
	ext_version    varchar(64) NOT NULL,
	platform       varchar(64) NOT NULL,
	duckdb_version varchar(64) NOT NULL,
	day            varchar(10) NOT NULL,
	authenticated  boolean NOT NULL,
	count          bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, platform, duckdb_version, day, authenticated)
)
-- +statement
CREATE INDEX ix_download_counts_day ON download_counts (tenant_id, day)
-- +statement
CREATE TABLE download_installers (
	tenant_id   varchar(36) NOT NULL,
	channel_id  varchar(36) NOT NULL,
	name        varchar(64) NOT NULL,
	ext_version varchar(64) NOT NULL,
	platform    varchar(64) NOT NULL,
	day         varchar(10) NOT NULL,
	installers  bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, platform, day)
)
-- +statement
CREATE INDEX ix_download_installers_day ON download_installers (tenant_id, day)
-- +statement
CREATE TABLE statistics_days (
	day     varchar(10) PRIMARY KEY,
	done_at timestamptz NOT NULL
)
