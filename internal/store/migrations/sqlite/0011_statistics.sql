-- spec 0010 phase 2: download counts (no principal, no address) and daily installers
CREATE TABLE download_counts (
	tenant_id      TEXT NOT NULL,
	channel_id     TEXT NOT NULL,
	name           TEXT NOT NULL,
	ext_version    TEXT NOT NULL,
	platform       TEXT NOT NULL,
	duckdb_version TEXT NOT NULL,
	day            TEXT NOT NULL,
	authenticated  INTEGER NOT NULL,
	count          INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, platform, duckdb_version, day, authenticated)
)
-- +statement
CREATE INDEX ix_download_counts_day ON download_counts (tenant_id, day)
-- +statement
CREATE TABLE download_installers (
	tenant_id   TEXT NOT NULL,
	channel_id  TEXT NOT NULL,
	name        TEXT NOT NULL,
	ext_version TEXT NOT NULL,
	platform    TEXT NOT NULL,
	day         TEXT NOT NULL,
	installers  INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, platform, day)
)
-- +statement
CREATE INDEX ix_download_installers_day ON download_installers (tenant_id, day)
-- +statement
CREATE TABLE statistics_days (
	day     TEXT PRIMARY KEY,
	done_at TEXT NOT NULL
)
