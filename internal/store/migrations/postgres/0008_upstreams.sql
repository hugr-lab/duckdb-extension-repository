-- +min_reader 8
CREATE TABLE upstreams (
	id              varchar(36) PRIMARY KEY,
	tenant_id       varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name            varchar(64) NOT NULL,
	kind            varchar(32) NOT NULL CHECK (kind IN ('duckdb-core', 'duckdb-community', 'repository')),
	prefix          varchar(1000),
	channel_id      varchar(36) NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	mode            varchar(32) NOT NULL CHECK (mode IN ('mirror', 'pull-through')),
	visibility      varchar(16) NOT NULL CHECK (visibility IN ('public', 'private')),
	state           varchar(16) NOT NULL CHECK (state IN ('active', 'paused')),
	version         bigint NOT NULL,
	requested_at    timestamptz,
	request_dry_run boolean NOT NULL,
	next_run_at     timestamptz,
	last_run_at     timestamptz,
	last_run        text,
	created_at      timestamptz NOT NULL,
	created_by      varchar(400) NOT NULL,
	UNIQUE (tenant_id, name)
)
-- +statement
CREATE INDEX ix_upstreams_channel ON upstreams (channel_id)
-- +statement
CREATE TABLE upstream_keys (
	upstream_id varchar(36) NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	fingerprint varchar(80) NOT NULL,
	PRIMARY KEY (upstream_id, fingerprint)
)
-- +statement
CREATE TABLE upstream_platforms (
	upstream_id varchar(36) NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	platform    varchar(64) NOT NULL,
	PRIMARY KEY (upstream_id, platform)
)
-- +statement
CREATE TABLE upstream_entries (
	upstream_id    varchar(36) NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	name           varchar(64) NOT NULL,
	allow_reserved boolean NOT NULL,
	PRIMARY KEY (upstream_id, name)
)
-- +statement
CREATE TABLE upstream_versions (
	upstream_id varchar(36) NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	name        varchar(64) NOT NULL,
	ext_version varchar(64) NOT NULL,
	PRIMARY KEY (upstream_id, name, ext_version)
)
-- +statement
CREATE TABLE upstream_cells (
	upstream_id    varchar(36) NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	duckdb_version varchar(64) NOT NULL,
	platform       varchar(64) NOT NULL,
	name           varchar(64) NOT NULL,
	etag           varchar(256),
	body_hash      varchar(64),
	outcome        varchar(32) NOT NULL,
	detail         varchar(1000),
	fetched_at     timestamptz NOT NULL,
	PRIMARY KEY (upstream_id, duckdb_version, platform, name)
)
-- +statement
CREATE TABLE shadows (
	tenant_id  varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name       varchar(64) NOT NULL,
	created_at timestamptz NOT NULL,
	created_by varchar(400) NOT NULL,
	PRIMARY KEY (tenant_id, name)
)
-- +statement
ALTER TABLE releases ADD COLUMN origin_signature bytea
