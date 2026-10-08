-- +min_reader 3
CREATE TABLE duckdb_version_c_apis (
	duckdb_version_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES duckdb_versions (id) ON DELETE NO ACTION,
	major             int NOT NULL CHECK (major >= 0),
	max_minor         int NOT NULL CHECK (max_minor >= 0),
	max_patch         int NOT NULL CHECK (max_patch >= 0),
	PRIMARY KEY (duckdb_version_id, major)
)
-- +statement
CREATE TABLE leases (
	name       nvarchar(200) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	holder     nvarchar(200) COLLATE Latin1_General_100_BIN2 NOT NULL,
	expires_at datetime2(6) NOT NULL,
	version    bigint NOT NULL
)
-- +statement
CREATE TABLE builds (
	id               nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id        nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name             nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ext_version      nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	platform         nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	abi              nvarchar(24) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (abi IN ('cpp', 'c_struct', 'c_struct_unstable')),
	duckdb_version   nvarchar(64) COLLATE Latin1_General_100_BIN2,
	c_api_major      int,
	c_api_minor      int,
	c_api_patch      int,
	body_hash        nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	origin           nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (origin IN ('admin', 'publication', 'upstream')),
	origin_signature varbinary(256),
	created_at       datetime2(6) NOT NULL,
	created_by       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	UNIQUE (tenant_id, name, body_hash),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE releases (
	id               nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id        nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id       nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	build_id         nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name             nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ext_version      nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	platform         nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	slot             nvarchar(72) COLLATE Latin1_General_100_BIN2 NOT NULL,
	state            nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('active', 'deprecated', 'yanked')),
	visibility       nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (visibility IN ('public', 'private')),
	seq              bigint,
	created_at       datetime2(6) NOT NULL,
	created_by       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	state_changed_at datetime2(6) NOT NULL,
	state_changed_by nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	version          bigint NOT NULL,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION,
	FOREIGN KEY (build_id, tenant_id) REFERENCES builds (id, tenant_id) ON DELETE NO ACTION,
	UNIQUE (channel_id, name, ext_version, platform, slot),
	UNIQUE (id, channel_id)
)
-- +statement
CREATE UNIQUE NONCLUSTERED INDEX ux_releases_seq ON releases (channel_id, seq) WHERE seq IS NOT NULL
-- +statement
CREATE INDEX ix_releases_resolve ON releases (channel_id, name, platform, state, seq)
-- +statement
CREATE INDEX ix_releases_build ON releases (build_id)
-- +statement
CREATE UNIQUE NONCLUSTERED INDEX ux_channel_keys_id_channel ON channel_keys (id, channel_id)
-- +statement
CREATE TABLE release_signatures (
	release_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	key_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	signature  varbinary(256) NOT NULL CHECK (datalength(signature) = 256),
	created_at datetime2(6) NOT NULL,
	PRIMARY KEY (release_id, key_id),
	FOREIGN KEY (release_id, channel_id) REFERENCES releases (id, channel_id) ON DELETE NO ACTION,
	FOREIGN KEY (key_id, channel_id) REFERENCES channel_keys (id, channel_id) ON DELETE NO ACTION
)
-- +statement
CREATE INDEX ix_release_signatures_key ON release_signatures (key_id)
-- +statement
ALTER TABLE channels ADD serving_key_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE channels ADD release_version bigint NOT NULL CONSTRAINT df_channels_release_version DEFAULT 0
-- +statement
ALTER TABLE channels ADD CONSTRAINT fk_channels_serving_key FOREIGN KEY (serving_key_id, id) REFERENCES channel_keys (id, channel_id) ON DELETE NO ACTION
