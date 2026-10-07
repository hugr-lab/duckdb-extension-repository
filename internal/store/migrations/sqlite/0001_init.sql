-- +min_reader 1
CREATE TABLE tenants (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL UNIQUE,
	display_name TEXT NOT NULL DEFAULT '',
	state        TEXT NOT NULL CHECK (state IN ('active', 'suspended')),
	created_at   TEXT NOT NULL,
	version      INTEGER NOT NULL
)
-- +statement
CREATE TABLE duckdb_versions (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL UNIQUE,
	kind          TEXT NOT NULL CHECK (kind IN ('release', 'dev')),
	c_api_version TEXT,
	created_at    TEXT NOT NULL
)
-- +statement
CREATE TABLE channels (
	id         TEXT PRIMARY KEY,
	tenant_id  TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name       TEXT NOT NULL,
	kind       TEXT NOT NULL CHECK (kind IN ('signed', 'passthrough')),
	created_at TEXT NOT NULL,
	version    INTEGER NOT NULL,
	UNIQUE (tenant_id, name),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE channel_duckdb_versions (
	channel_id        TEXT NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	duckdb_version_id TEXT NOT NULL REFERENCES duckdb_versions (id) ON DELETE NO ACTION,
	PRIMARY KEY (channel_id, duckdb_version_id)
)
-- +statement
CREATE INDEX ix_channel_duckdb_versions_version ON channel_duckdb_versions (duckdb_version_id)
-- +statement
CREATE TABLE key_fingerprints (
	fingerprint TEXT PRIMARY KEY,
	key_id      TEXT NOT NULL
)
-- +statement
CREATE TABLE channel_keys (
	id               TEXT PRIMARY KEY,
	tenant_id        TEXT NOT NULL,
	channel_id       TEXT NOT NULL,
	fingerprint      TEXT NOT NULL UNIQUE REFERENCES key_fingerprints (fingerprint) ON DELETE NO ACTION,
	signer_ref       TEXT NOT NULL,
	public_key       BLOB NOT NULL,
	state            TEXT NOT NULL CHECK (state IN ('trusted', 'active', 'retired')),
	trusted_since    TEXT NOT NULL,
	state_changed_at TEXT NOT NULL,
	created_at       TEXT NOT NULL,
	created_by       TEXT NOT NULL,
	state_changed_by TEXT NOT NULL,
	version          INTEGER NOT NULL,
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
	id         TEXT PRIMARY KEY,
	key_id     TEXT NOT NULL REFERENCES channel_keys (id) ON DELETE NO ACTION,
	from_state TEXT,
	to_state   TEXT NOT NULL,
	actor      TEXT NOT NULL,
	forced     INTEGER NOT NULL,
	at         TEXT NOT NULL
)
-- +statement
CREATE INDEX ix_key_events_key ON key_events (key_id)
