-- +min_reader 8
CREATE TABLE upstreams (
	id              nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id       nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name            nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	kind            nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (kind IN ('duckdb-core', 'duckdb-community', 'repository')),
	prefix          nvarchar(1000) COLLATE Latin1_General_100_BIN2 NULL,
	channel_id      nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	mode            nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (mode IN ('mirror', 'pull-through')),
	visibility      nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (visibility IN ('public', 'private')),
	state           nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('active', 'paused')),
	version         bigint NOT NULL,
	requested_at    datetime2(6) NULL,
	request_dry_run bit NOT NULL,
	next_run_at     datetime2(6) NULL,
	last_run_at     datetime2(6) NULL,
	last_run        nvarchar(max) NULL,
	created_at      datetime2(6) NOT NULL,
	created_by      nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	UNIQUE (tenant_id, name)
)
-- +statement
CREATE INDEX ix_upstreams_channel ON upstreams (channel_id)
-- +statement
CREATE TABLE upstream_keys (
	upstream_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	fingerprint nvarchar(80) COLLATE Latin1_General_100_BIN2 NOT NULL,
	PRIMARY KEY (upstream_id, fingerprint)
)
-- +statement
CREATE TABLE upstream_platforms (
	upstream_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	platform    nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	PRIMARY KEY (upstream_id, platform)
)
-- +statement
CREATE TABLE upstream_entries (
	upstream_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	name           nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	allow_reserved bit NOT NULL,
	PRIMARY KEY (upstream_id, name)
)
-- +statement
CREATE TABLE upstream_versions (
	upstream_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	name        nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ext_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	PRIMARY KEY (upstream_id, name, ext_version)
)
-- +statement
CREATE TABLE upstream_cells (
	upstream_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	duckdb_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	platform       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name           nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	etag           nvarchar(256) COLLATE Latin1_General_100_BIN2 NULL,
	body_hash      nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL,
	outcome        nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	detail         nvarchar(1000) COLLATE Latin1_General_100_BIN2 NULL,
	fetched_at     datetime2(6) NOT NULL,
	PRIMARY KEY (upstream_id, duckdb_version, platform, name)
)
-- +statement
CREATE TABLE shadows (
	tenant_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	created_at datetime2(6) NOT NULL,
	created_by nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	PRIMARY KEY (tenant_id, name)
)
-- +statement
ALTER TABLE releases ADD origin_signature varbinary(256) NULL
