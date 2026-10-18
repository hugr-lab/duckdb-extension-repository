-- spec 0012 phase 1b: usage reports' counts (no principal, no address) and daily users
CREATE TABLE usage_counts (
	tenant_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name          nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ext_version   nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	event         nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	counter       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	status        nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	day           nvarchar(10) COLLATE Latin1_General_100_BIN2 NOT NULL,
	authenticated bit NOT NULL,
	value         bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, event, counter, status, day, authenticated)
)
-- +statement
CREATE INDEX ix_usage_counts_day ON usage_counts (tenant_id, day)
-- +statement
CREATE TABLE usage_users (
	tenant_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name        nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ext_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	day         nvarchar(10) COLLATE Latin1_General_100_BIN2 NOT NULL,
	users       bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, day)
)
-- +statement
CREATE INDEX ix_usage_users_day ON usage_users (tenant_id, day)
