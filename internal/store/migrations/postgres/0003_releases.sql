-- +min_reader 3
CREATE TABLE duckdb_version_c_apis (
	duckdb_version_id varchar(36) NOT NULL REFERENCES duckdb_versions (id) ON DELETE NO ACTION,
	major             int NOT NULL CHECK (major >= 0),
	max_minor         int NOT NULL CHECK (max_minor >= 0),
	max_patch         int NOT NULL CHECK (max_patch >= 0),
	PRIMARY KEY (duckdb_version_id, major)
)
-- +statement
CREATE TABLE leases (
	name       varchar(200) PRIMARY KEY,
	holder     varchar(200) NOT NULL,
	expires_at timestamptz NOT NULL,
	version    bigint NOT NULL
)
-- +statement
CREATE TABLE builds (
	id               varchar(36) PRIMARY KEY,
	tenant_id        varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name             varchar(64) NOT NULL,
	ext_version      varchar(64) NOT NULL,
	platform         varchar(64) NOT NULL,
	abi              varchar(24) NOT NULL CHECK (abi IN ('cpp', 'c_struct', 'c_struct_unstable')),
	duckdb_version   varchar(64),
	c_api_major      int,
	c_api_minor      int,
	c_api_patch      int,
	body_hash        varchar(64) NOT NULL,
	origin           varchar(16) NOT NULL CHECK (origin IN ('admin', 'publication', 'upstream')),
	origin_signature bytea,
	created_at       timestamptz NOT NULL,
	created_by       varchar(400) NOT NULL,
	UNIQUE (tenant_id, name, body_hash),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE releases (
	id               varchar(36) PRIMARY KEY,
	tenant_id        varchar(36) NOT NULL,
	channel_id       varchar(36) NOT NULL,
	build_id         varchar(36) NOT NULL,
	name             varchar(64) NOT NULL,
	ext_version      varchar(64) NOT NULL,
	platform         varchar(64) NOT NULL,
	slot             varchar(72) NOT NULL,
	state            varchar(16) NOT NULL CHECK (state IN ('active', 'deprecated', 'yanked')),
	visibility       varchar(16) NOT NULL CHECK (visibility IN ('public', 'private')),
	seq              bigint,
	created_at       timestamptz NOT NULL,
	created_by       varchar(400) NOT NULL,
	state_changed_at timestamptz NOT NULL,
	state_changed_by varchar(400) NOT NULL,
	version          bigint NOT NULL,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION,
	FOREIGN KEY (build_id, tenant_id) REFERENCES builds (id, tenant_id) ON DELETE NO ACTION,
	UNIQUE (channel_id, name, ext_version, platform, slot),
	UNIQUE (id, channel_id)
)
-- +statement
CREATE UNIQUE INDEX ux_releases_seq ON releases (channel_id, seq) WHERE seq IS NOT NULL
-- +statement
CREATE INDEX ix_releases_resolve ON releases (channel_id, name, platform, state, seq)
-- +statement
CREATE INDEX ix_releases_build ON releases (build_id)
-- +statement
CREATE UNIQUE INDEX ux_channel_keys_id_channel ON channel_keys (id, channel_id)
-- +statement
CREATE TABLE release_signatures (
	release_id varchar(36) NOT NULL,
	channel_id varchar(36) NOT NULL,
	key_id     varchar(36) NOT NULL,
	signature  bytea NOT NULL CHECK (octet_length(signature) = 256),
	created_at timestamptz NOT NULL,
	PRIMARY KEY (release_id, key_id),
	FOREIGN KEY (release_id, channel_id) REFERENCES releases (id, channel_id) ON DELETE NO ACTION,
	FOREIGN KEY (key_id, channel_id) REFERENCES channel_keys (id, channel_id) ON DELETE NO ACTION
)
-- +statement
CREATE INDEX ix_release_signatures_key ON release_signatures (key_id)
-- +statement
ALTER TABLE channels ADD COLUMN serving_key_id varchar(36)
-- +statement
ALTER TABLE channels ADD COLUMN release_version bigint NOT NULL DEFAULT 0
-- +statement
ALTER TABLE channels ADD CONSTRAINT fk_channels_serving_key FOREIGN KEY (serving_key_id, id) REFERENCES channel_keys (id, channel_id) ON DELETE NO ACTION
