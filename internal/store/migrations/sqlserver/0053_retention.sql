-- +min_reader 53
-- spec 0042: version retention in channels. A release gains the state retired (not served, restored
-- by an administrator until the grace after which the retention pass purges it), with the state it
-- came from and when; a tenant's default rule and a channel's own (keep the newest N versions per
-- DuckDB version, purge after a grace), and the pins that keep a version whatever the rule. An older
-- replica does not know the retired state (it would refuse or misread it), and its tenant reaper does
-- not know retention_pins, which references channels, so it no longer runs here. The state's CHECK,
-- unnamed in migration 0003, has a generated name: found by its definition.
DECLARE @n sysname = (SELECT name FROM sys.check_constraints WHERE parent_object_id = OBJECT_ID('releases') AND definition LIKE '%yanked%');
IF @n IS NULL THROW 50000, 'migration 0053: the releases state check was not found', 1;
DECLARE @q nvarchar(300) = N'ALTER TABLE releases DROP CONSTRAINT ' + QUOTENAME(@n);
EXEC (@q)
-- +statement
ALTER TABLE releases ADD CONSTRAINT ck_releases_state CHECK (state IN ('active', 'deprecated', 'yanked', 'retired'))
-- +statement
ALTER TABLE releases ADD retired_from nvarchar(16) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE releases ADD retired_at datetime2(6) NULL
-- +statement
-- a rule's fields: NULL is not set; a channel's keep 0 is none there, whatever the tenant's
ALTER TABLE tenants ADD retention_keep int NULL
-- +statement
ALTER TABLE tenants ADD retention_grace_seconds bigint NULL
-- +statement
ALTER TABLE channels ADD retention_keep int NULL
-- +statement
ALTER TABLE channels ADD retention_grace_seconds bigint NULL
-- +statement
ALTER TABLE channels ADD retention_checked_at datetime2(6) NULL
-- +statement
-- a pin keeps (name, version) in one DuckDB version of the channel, or in every one ('')
CREATE TABLE retention_pins (
	id              nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	channel_id      nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES channels (id),
	name            nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	package_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	duckdb_version  nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	reason          nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	pinned_by       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	pinned_at       datetime2(6) NOT NULL,
	CONSTRAINT ux_retention_pins UNIQUE (channel_id, name, package_version, duckdb_version)
)
