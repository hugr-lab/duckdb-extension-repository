-- +min_reader 1
CREATE TABLE tenants (
	id           nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	name         nvarchar(63) COLLATE Latin1_General_100_BIN2 NOT NULL UNIQUE,
	display_name nvarchar(200) COLLATE Latin1_General_100_BIN2 NOT NULL DEFAULT N'',
	state        nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('active', 'suspended')),
	created_at   datetime2(6) NOT NULL,
	version      bigint NOT NULL
)
-- +statement
CREATE TABLE duckdb_versions (
	id            nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	name          nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL UNIQUE,
	kind          nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (kind IN ('release', 'dev')),
	c_api_version nvarchar(32) COLLATE Latin1_General_100_BIN2,
	created_at    datetime2(6) NOT NULL
)
-- +statement
CREATE TABLE channels (
	id         nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name       nvarchar(63) COLLATE Latin1_General_100_BIN2 NOT NULL,
	kind       nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (kind IN ('signed', 'passthrough')),
	created_at datetime2(6) NOT NULL,
	version    bigint NOT NULL,
	UNIQUE (tenant_id, name),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE channel_duckdb_versions (
	channel_id        nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	duckdb_version_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES duckdb_versions (id) ON DELETE NO ACTION,
	PRIMARY KEY (channel_id, duckdb_version_id)
)
-- +statement
CREATE INDEX ix_channel_duckdb_versions_version ON channel_duckdb_versions (duckdb_version_id)
-- +statement
CREATE TABLE key_fingerprints (
	fingerprint nvarchar(80) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	key_id      nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL
)
-- +statement
CREATE TABLE channel_keys (
	id               nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id        nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id       nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	fingerprint      nvarchar(80) COLLATE Latin1_General_100_BIN2 NOT NULL UNIQUE REFERENCES key_fingerprints (fingerprint) ON DELETE NO ACTION,
	signer_ref       nvarchar(2048) COLLATE Latin1_General_100_BIN2 NOT NULL,
	public_key       varbinary(1024) NOT NULL,
	state            nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('trusted', 'active', 'retired')),
	trusted_since    datetime2(6) NOT NULL,
	state_changed_at datetime2(6) NOT NULL,
	created_at       datetime2(6) NOT NULL,
	created_by       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	state_changed_by nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	version          bigint NOT NULL,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION
)
-- +statement
CREATE UNIQUE NONCLUSTERED INDEX ux_channel_keys_active ON channel_keys (channel_id) WHERE state = N'active'
-- +statement
CREATE INDEX ix_channel_keys_channel ON channel_keys (channel_id)
-- +statement
CREATE INDEX ix_channel_keys_tenant ON channel_keys (tenant_id)
-- +statement
CREATE TABLE key_events (
	id         nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	key_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES channel_keys (id) ON DELETE NO ACTION,
	from_state nvarchar(16) COLLATE Latin1_General_100_BIN2,
	to_state   nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	actor      nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	forced     bit NOT NULL,
	at         datetime2(6) NOT NULL
)
-- +statement
CREATE INDEX ix_key_events_key ON key_events (key_id)
