-- +min_reader 3
CREATE TABLE duckdb_version_c_apis (
	duckdb_version_id TEXT NOT NULL REFERENCES duckdb_versions (id) ON DELETE NO ACTION,
	major             INTEGER NOT NULL CHECK (major >= 0),
	max_minor         INTEGER NOT NULL CHECK (max_minor >= 0),
	max_patch         INTEGER NOT NULL CHECK (max_patch >= 0),
	PRIMARY KEY (duckdb_version_id, major)
)
-- +statement
CREATE TABLE leases (
	name       TEXT PRIMARY KEY,
	holder     TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	version    INTEGER NOT NULL
)
-- +statement
CREATE TABLE builds (
	id               TEXT PRIMARY KEY,
	tenant_id        TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name             TEXT NOT NULL,
	ext_version      TEXT NOT NULL,
	platform         TEXT NOT NULL,
	abi              TEXT NOT NULL CHECK (abi IN ('cpp', 'c_struct', 'c_struct_unstable')),
	duckdb_version   TEXT,
	c_api_major      INTEGER,
	c_api_minor      INTEGER,
	c_api_patch      INTEGER,
	body_hash        TEXT NOT NULL,
	origin           TEXT NOT NULL CHECK (origin IN ('admin', 'publication', 'upstream')),
	origin_signature BLOB,
	created_at       TEXT NOT NULL,
	created_by       TEXT NOT NULL,
	UNIQUE (tenant_id, name, body_hash),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE releases (
	id               TEXT PRIMARY KEY,
	tenant_id        TEXT NOT NULL,
	channel_id       TEXT NOT NULL,
	build_id         TEXT NOT NULL,
	name             TEXT NOT NULL,
	ext_version      TEXT NOT NULL,
	platform         TEXT NOT NULL,
	slot             TEXT NOT NULL,
	state            TEXT NOT NULL CHECK (state IN ('active', 'deprecated', 'yanked')),
	visibility       TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
	seq              INTEGER,
	created_at       TEXT NOT NULL,
	created_by       TEXT NOT NULL,
	state_changed_at TEXT NOT NULL,
	state_changed_by TEXT NOT NULL,
	version          INTEGER NOT NULL,
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
	release_id TEXT NOT NULL,
	channel_id TEXT NOT NULL,
	key_id     TEXT NOT NULL,
	signature  BLOB NOT NULL CHECK (length(signature) = 256),
	created_at TEXT NOT NULL,
	PRIMARY KEY (release_id, key_id),
	FOREIGN KEY (release_id, channel_id) REFERENCES releases (id, channel_id) ON DELETE NO ACTION,
	FOREIGN KEY (key_id, channel_id) REFERENCES channel_keys (id, channel_id) ON DELETE NO ACTION
)
-- +statement
CREATE INDEX ix_release_signatures_key ON release_signatures (key_id)
-- +statement
ALTER TABLE channels ADD COLUMN serving_key_id TEXT REFERENCES channel_keys (id)
-- +statement
ALTER TABLE channels ADD COLUMN release_version INTEGER NOT NULL DEFAULT 0
