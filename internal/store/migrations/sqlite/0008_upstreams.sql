-- +min_reader 8
CREATE TABLE upstreams (
	id              TEXT PRIMARY KEY,
	tenant_id       TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name            TEXT NOT NULL,
	kind            TEXT NOT NULL CHECK (kind IN ('duckdb-core', 'duckdb-community', 'repository')),
	prefix          TEXT,
	channel_id      TEXT NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	mode            TEXT NOT NULL CHECK (mode IN ('mirror', 'pull-through')),
	visibility      TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
	state           TEXT NOT NULL CHECK (state IN ('active', 'paused')),
	version         INTEGER NOT NULL,
	requested_at    TEXT,
	request_dry_run INTEGER NOT NULL,
	next_run_at     TEXT,
	last_run_at     TEXT,
	last_run        TEXT,
	created_at      TEXT NOT NULL,
	created_by      TEXT NOT NULL,
	UNIQUE (tenant_id, name)
)
-- +statement
CREATE INDEX ix_upstreams_channel ON upstreams (channel_id)
-- +statement
CREATE TABLE upstream_keys (
	upstream_id TEXT NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	fingerprint TEXT NOT NULL,
	PRIMARY KEY (upstream_id, fingerprint)
)
-- +statement
CREATE TABLE upstream_platforms (
	upstream_id TEXT NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	platform    TEXT NOT NULL,
	PRIMARY KEY (upstream_id, platform)
)
-- +statement
CREATE TABLE upstream_entries (
	upstream_id    TEXT NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	name           TEXT NOT NULL,
	allow_reserved INTEGER NOT NULL,
	PRIMARY KEY (upstream_id, name)
)
-- +statement
CREATE TABLE upstream_versions (
	upstream_id TEXT NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	ext_version TEXT NOT NULL,
	PRIMARY KEY (upstream_id, name, ext_version)
)
-- +statement
CREATE TABLE upstream_cells (
	upstream_id    TEXT NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
	duckdb_version TEXT NOT NULL,
	platform       TEXT NOT NULL,
	name           TEXT NOT NULL,
	etag           TEXT,
	body_hash      TEXT,
	outcome        TEXT NOT NULL,
	detail         TEXT,
	fetched_at     TEXT NOT NULL,
	PRIMARY KEY (upstream_id, duckdb_version, platform, name)
)
-- +statement
CREATE TABLE shadows (
	tenant_id  TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL,
	PRIMARY KEY (tenant_id, name)
)
-- +statement
ALTER TABLE releases ADD COLUMN origin_signature BLOB
