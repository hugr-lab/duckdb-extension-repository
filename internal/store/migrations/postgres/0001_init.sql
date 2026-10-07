-- +min_reader 1
CREATE TABLE tenants (
	id           varchar(36) PRIMARY KEY,
	name         varchar(63) NOT NULL UNIQUE,
	display_name varchar(200) NOT NULL DEFAULT '',
	state        varchar(16) NOT NULL CHECK (state IN ('active', 'suspended')),
	created_at   timestamptz NOT NULL,
	version      bigint NOT NULL
)
-- +statement
CREATE TABLE duckdb_versions (
	id            varchar(36) PRIMARY KEY,
	name          varchar(64) NOT NULL UNIQUE,
	kind          varchar(16) NOT NULL CHECK (kind IN ('release', 'dev')),
	c_api_version varchar(32),
	created_at    timestamptz NOT NULL
)
-- +statement
CREATE TABLE channels (
	id         varchar(36) PRIMARY KEY,
	tenant_id  varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name       varchar(63) NOT NULL,
	kind       varchar(16) NOT NULL CHECK (kind IN ('signed', 'passthrough')),
	created_at timestamptz NOT NULL,
	version    bigint NOT NULL,
	UNIQUE (tenant_id, name),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE channel_duckdb_versions (
	channel_id        varchar(36) NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	duckdb_version_id varchar(36) NOT NULL REFERENCES duckdb_versions (id) ON DELETE NO ACTION,
	PRIMARY KEY (channel_id, duckdb_version_id)
)
-- +statement
CREATE INDEX ix_channel_duckdb_versions_version ON channel_duckdb_versions (duckdb_version_id)
-- +statement
CREATE TABLE key_fingerprints (
	fingerprint varchar(80) PRIMARY KEY,
	key_id      varchar(36) NOT NULL
)
-- +statement
CREATE TABLE channel_keys (
	id               varchar(36) PRIMARY KEY,
	tenant_id        varchar(36) NOT NULL,
	channel_id       varchar(36) NOT NULL,
	fingerprint      varchar(80) NOT NULL UNIQUE REFERENCES key_fingerprints (fingerprint) ON DELETE NO ACTION,
	signer_ref       varchar(2048) NOT NULL,
	public_key       bytea NOT NULL,
	state            varchar(16) NOT NULL CHECK (state IN ('trusted', 'active', 'retired')),
	trusted_since    timestamptz NOT NULL,
	state_changed_at timestamptz NOT NULL,
	created_at       timestamptz NOT NULL,
	created_by       varchar(400) NOT NULL,
	state_changed_by varchar(400) NOT NULL,
	version          bigint NOT NULL,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION
)
-- +statement
CREATE UNIQUE INDEX ux_channel_keys_active ON channel_keys (channel_id) WHERE state = 'active'
-- +statement
CREATE INDEX ix_channel_keys_channel ON channel_keys (channel_id)
-- +statement
CREATE INDEX ix_channel_keys_tenant ON channel_keys (tenant_id)
-- +statement
CREATE TABLE key_events (
	id         varchar(36) PRIMARY KEY,
	key_id     varchar(36) NOT NULL REFERENCES channel_keys (id) ON DELETE NO ACTION,
	from_state varchar(16),
	to_state   varchar(16) NOT NULL,
	actor      varchar(400) NOT NULL,
	forced     boolean NOT NULL,
	at         timestamptz NOT NULL
)
-- +statement
CREATE INDEX ix_key_events_key ON key_events (key_id)
