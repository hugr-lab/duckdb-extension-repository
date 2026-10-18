-- spec 0012 phase 1b: usage reports' counts (no principal, no address) and daily users
CREATE TABLE usage_counts (
	tenant_id     varchar(36) NOT NULL,
	channel_id    varchar(36) NOT NULL,
	name          varchar(64) NOT NULL,
	ext_version   varchar(64) NOT NULL,
	event         varchar(64) NOT NULL,
	counter       varchar(64) NOT NULL,
	status        varchar(16) NOT NULL,
	day           varchar(10) NOT NULL,
	authenticated boolean NOT NULL,
	value         bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, event, counter, status, day, authenticated)
)
-- +statement
CREATE INDEX ix_usage_counts_day ON usage_counts (tenant_id, day)
-- +statement
CREATE TABLE usage_users (
	tenant_id   varchar(36) NOT NULL,
	channel_id  varchar(36) NOT NULL,
	name        varchar(64) NOT NULL,
	ext_version varchar(64) NOT NULL,
	day         varchar(10) NOT NULL,
	users       bigint NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, day)
)
-- +statement
CREATE INDEX ix_usage_users_day ON usage_users (tenant_id, day)
