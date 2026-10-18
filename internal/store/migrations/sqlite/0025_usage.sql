-- spec 0012 phase 1b: usage reports' counts (no principal, no address) and daily users
CREATE TABLE usage_counts (
	tenant_id     TEXT NOT NULL,
	channel_id    TEXT NOT NULL,
	name          TEXT NOT NULL,
	ext_version   TEXT NOT NULL,
	event         TEXT NOT NULL,
	counter       TEXT NOT NULL,
	status        TEXT NOT NULL,
	day           TEXT NOT NULL,
	authenticated INTEGER NOT NULL,
	value         INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, event, counter, status, day, authenticated)
)
-- +statement
CREATE INDEX ix_usage_counts_day ON usage_counts (tenant_id, day)
-- +statement
CREATE TABLE usage_users (
	tenant_id   TEXT NOT NULL,
	channel_id  TEXT NOT NULL,
	name        TEXT NOT NULL,
	ext_version TEXT NOT NULL,
	day         TEXT NOT NULL,
	users       INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, channel_id, name, ext_version, day)
)
-- +statement
CREATE INDEX ix_usage_users_day ON usage_users (tenant_id, day)
