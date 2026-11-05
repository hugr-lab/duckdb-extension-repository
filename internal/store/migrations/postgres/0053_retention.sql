-- +min_reader 53
-- spec 0042: version retention in channels. A release gains the state retired (not served, restored
-- by an administrator until the grace after which the retention pass purges it), with the state it
-- came from and when; a tenant's default rule and a channel's own (keep the newest N versions per
-- DuckDB version, purge after a grace), and the pins that keep a version whatever the rule. An older
-- replica does not know the retired state (it would refuse or misread it), and its tenant reaper does
-- not know retention_pins, which references channels, so it no longer runs here. The state's CHECK,
-- unnamed in migration 0003, has PostgreSQL's generated name.
ALTER TABLE releases DROP CONSTRAINT releases_state_check
-- +statement
ALTER TABLE releases ADD CONSTRAINT releases_state_check CHECK (state IN ('active', 'deprecated', 'yanked', 'retired'))
-- +statement
ALTER TABLE releases ADD COLUMN retired_from varchar(16) NULL
-- +statement
ALTER TABLE releases ADD COLUMN retired_at timestamptz NULL
-- +statement
-- a rule's fields: NULL is not set; a channel's keep 0 is none there, whatever the tenant's
ALTER TABLE tenants ADD COLUMN retention_keep integer NULL
-- +statement
ALTER TABLE tenants ADD COLUMN retention_grace_seconds bigint NULL
-- +statement
ALTER TABLE channels ADD COLUMN retention_keep integer NULL
-- +statement
ALTER TABLE channels ADD COLUMN retention_grace_seconds bigint NULL
-- +statement
ALTER TABLE channels ADD COLUMN retention_checked_at timestamptz NULL
-- +statement
-- a pin keeps (name, version) in one DuckDB version of the channel, or in every one ('')
CREATE TABLE retention_pins (
	id              varchar(36) PRIMARY KEY,
	channel_id      varchar(36) NOT NULL REFERENCES channels (id),
	name            varchar(64) NOT NULL,
	package_version varchar(64) NOT NULL,
	duckdb_version  varchar(64) NOT NULL,
	reason          varchar(400) NOT NULL,
	pinned_by       varchar(400) NOT NULL,
	pinned_at       timestamptz NOT NULL,
	CONSTRAINT ux_retention_pins UNIQUE (channel_id, name, package_version, duckdb_version)
)
