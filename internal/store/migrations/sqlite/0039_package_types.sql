-- +min_reader 39
-- +foreign_keys_off
-- spec 0034 phase 2: a package is a name of one type, and every key that holds a name holds the
-- pair (type, name), directly or through a channel. Every existing row is duckdb_extension, the one
-- type there is (a grant without a name is any type, '*'), so every channel serves what it served and
-- every key holds what it held. An older replica's upserts would fail against the new keys, and it
-- must not read rows of a type it does not know, so it no longer runs here. SQLite rebuilds the
-- tables whose keys or checks change, with foreign keys off and checked before the commit.
ALTER TABLE channels ADD COLUMN package_type TEXT NOT NULL DEFAULT 'duckdb_extension'
-- +statement
CREATE TABLE builds_new (
	id               TEXT PRIMARY KEY,
	tenant_id        TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type     TEXT NOT NULL DEFAULT 'duckdb_extension',
	source           TEXT NOT NULL,
	name             TEXT NOT NULL,
	package_version  TEXT NOT NULL,
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
	checked          INTEGER NOT NULL DEFAULT 0,
	used_at          TEXT,
	visibility       TEXT NOT NULL DEFAULT 'private',
	provenance       TEXT,
	landed           INTEGER NOT NULL DEFAULT 0,
	slot             TEXT NOT NULL DEFAULT '',
	legacy_duplicate INTEGER NOT NULL DEFAULT 0,
	CONSTRAINT ux_builds_source UNIQUE (tenant_id, package_type, source, name, body_hash),
	UNIQUE (id, tenant_id)
)
-- +statement
INSERT INTO builds_new (id, tenant_id, package_type, source, name, package_version, platform, abi, duckdb_version, c_api_major,
	c_api_minor, c_api_patch, body_hash, origin, origin_signature, created_at, created_by, checked, used_at, visibility, provenance,
	landed, slot, legacy_duplicate)
SELECT id, tenant_id, 'duckdb_extension', source, name, package_version, platform, abi, duckdb_version, c_api_major, c_api_minor,
	c_api_patch, body_hash, origin, origin_signature, created_at, created_by, checked, used_at, visibility, provenance, landed, slot,
	legacy_duplicate
FROM builds
-- +statement
DROP TABLE builds
-- +statement
ALTER TABLE builds_new RENAME TO builds
-- +statement
CREATE INDEX ix_builds_body ON builds (body_hash)
-- +statement
CREATE INDEX ix_builds_used ON builds (used_at)
-- +statement
CREATE UNIQUE INDEX ux_builds_slot ON builds (tenant_id, package_type, source, name, package_version, platform, slot)
WHERE legacy_duplicate = 0 AND source <> 'tenant'
-- +statement
CREATE INDEX ix_builds_source_name ON builds (tenant_id, package_type, source, name, created_at)
-- +statement
CREATE INDEX ix_builds_source_created ON builds (tenant_id, package_type, source, created_at)
-- +statement
-- a grant that names a package names its type; one without a name is any type ('*') or one type
CREATE TABLE grants_new (
	id           TEXT PRIMARY KEY,
	tenant_id    TEXT NOT NULL,
	issuer_id    TEXT,
	publisher_id TEXT,
	kind         TEXT NOT NULL CHECK (kind IN ('subject', 'role', 'group', 'client', 'issuer', 'publisher')),
	value        TEXT NOT NULL,
	channel_id   TEXT,
	package_type TEXT NOT NULL,
	package      TEXT,
	verbs        TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	created_by   TEXT NOT NULL,
	CHECK ((issuer_id IS NULL) <> (publisher_id IS NULL)),
	CHECK ((kind = 'publisher') = (publisher_id IS NOT NULL)),
	CONSTRAINT ck_grants_package_type CHECK (package IS NULL OR package_type <> '*'),
	FOREIGN KEY (issuer_id, tenant_id) REFERENCES issuers (id, tenant_id) ON DELETE NO ACTION,
	FOREIGN KEY (publisher_id, tenant_id) REFERENCES publishers (id, tenant_id) ON DELETE CASCADE,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION
)
-- +statement
INSERT INTO grants_new (id, tenant_id, issuer_id, publisher_id, kind, value, channel_id, package_type, package, verbs, created_at,
	created_by)
SELECT id, tenant_id, issuer_id, publisher_id, kind, value, channel_id, CASE WHEN package IS NULL THEN '*' ELSE 'duckdb_extension' END,
	package, verbs, created_at, created_by
FROM grants
-- +statement
DROP TABLE grants
-- +statement
ALTER TABLE grants_new RENAME TO grants
-- +statement
CREATE INDEX ix_grants_tenant ON grants (tenant_id)
-- +statement
CREATE INDEX ix_grants_issuer ON grants (issuer_id)
-- +statement
CREATE INDEX ix_grants_publisher ON grants (publisher_id)
-- +statement
CREATE TABLE package_group_members_new (
	group_id     TEXT NOT NULL,
	tenant_id    TEXT NOT NULL,
	package_type TEXT NOT NULL DEFAULT 'duckdb_extension',
	package      TEXT NOT NULL,
	added_at     TEXT NOT NULL,
	added_by     TEXT NOT NULL,
	CONSTRAINT pk_package_group_members PRIMARY KEY (group_id, package_type, package),
	CONSTRAINT fk_package_group_members_group FOREIGN KEY (group_id, tenant_id) REFERENCES package_groups (id, tenant_id) ON DELETE CASCADE
)
-- +statement
INSERT INTO package_group_members_new (group_id, tenant_id, package_type, package, added_at, added_by)
SELECT group_id, tenant_id, 'duckdb_extension', package, added_at, added_by FROM package_group_members
-- +statement
DROP TABLE package_group_members
-- +statement
ALTER TABLE package_group_members_new RENAME TO package_group_members
-- +statement
CREATE INDEX ix_package_group_members_tenant ON package_group_members (tenant_id, package_type, package)
-- +statement
-- the server's reserved names are per type: DuckDB's core and community names are duckdb_extension's
CREATE TABLE reserved_names_new (
	package_type TEXT NOT NULL DEFAULT 'duckdb_extension',
	name         TEXT NOT NULL,
	kind         TEXT NOT NULL,
	added_at     TEXT NOT NULL,
	CONSTRAINT pk_reserved_names PRIMARY KEY (package_type, name)
)
-- +statement
INSERT INTO reserved_names_new (package_type, name, kind, added_at) SELECT 'duckdb_extension', name, kind, added_at FROM reserved_names
-- +statement
CREATE TABLE reserved_name_sources_new (
	package_type TEXT NOT NULL DEFAULT 'duckdb_extension',
	name         TEXT NOT NULL,
	source       TEXT NOT NULL,
	added_at     TEXT NOT NULL,
	CONSTRAINT pk_reserved_name_sources PRIMARY KEY (package_type, name, source),
	CONSTRAINT fk_reserved_name_sources_name FOREIGN KEY (package_type, name) REFERENCES reserved_names (package_type, name)
)
-- +statement
INSERT INTO reserved_name_sources_new (package_type, name, source, added_at)
SELECT 'duckdb_extension', name, source, added_at FROM reserved_name_sources
-- +statement
DROP TABLE reserved_name_sources
-- +statement
DROP TABLE reserved_names
-- +statement
ALTER TABLE reserved_names_new RENAME TO reserved_names
-- +statement
ALTER TABLE reserved_name_sources_new RENAME TO reserved_name_sources
-- +statement
CREATE TABLE tenant_reserved_names_new (
	tenant_id    TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type TEXT NOT NULL DEFAULT 'duckdb_extension',
	name         TEXT NOT NULL,
	holder       TEXT NOT NULL,
	added_at     TEXT NOT NULL,
	added_by     TEXT NOT NULL,
	CONSTRAINT pk_tenant_reserved_names PRIMARY KEY (tenant_id, package_type, name)
)
-- +statement
INSERT INTO tenant_reserved_names_new (tenant_id, package_type, name, holder, added_at, added_by)
SELECT tenant_id, 'duckdb_extension', name, holder, added_at, added_by FROM tenant_reserved_names
-- +statement
DROP TABLE tenant_reserved_names
-- +statement
ALTER TABLE tenant_reserved_names_new RENAME TO tenant_reserved_names
-- +statement
CREATE TABLE licenses_new (
	tenant_id    TEXT NOT NULL REFERENCES tenants (id),
	package_type TEXT NOT NULL DEFAULT 'duckdb_extension',
	name         TEXT NOT NULL,
	license_id   TEXT NOT NULL,
	issuer       TEXT NOT NULL,
	holder_id    TEXT NOT NULL,
	holder       TEXT NOT NULL,
	plan_name    TEXT NOT NULL,
	token        TEXT NOT NULL,
	key_fp       TEXT NOT NULL,
	issued_at    INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	grace_until  INTEGER NOT NULL,
	source       TEXT NOT NULL CHECK (source IN ('import', 'upstream')),
	upstream_id  TEXT NULL,
	visibility   TEXT NOT NULL CHECK (visibility IN ('private', 'public')),
	actor        TEXT NOT NULL,
	updated_at   TEXT NOT NULL,
	version      INTEGER NOT NULL,
	CONSTRAINT pk_licenses PRIMARY KEY (tenant_id, package_type, name)
)
-- +statement
INSERT INTO licenses_new (tenant_id, package_type, name, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at,
	expires_at, grace_until, source, upstream_id, visibility, actor, updated_at, version)
SELECT tenant_id, 'duckdb_extension', name, license_id, issuer, holder_id, holder, plan_name, token, key_fp, issued_at, expires_at,
	grace_until, source, upstream_id, visibility, actor, updated_at, version
FROM licenses
-- +statement
DROP TABLE licenses
-- +statement
ALTER TABLE licenses_new RENAME TO licenses
-- +statement
CREATE INDEX ix_licenses_upstream ON licenses (upstream_id)
