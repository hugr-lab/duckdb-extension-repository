-- +min_reader 39
-- spec 0034 phase 2: a package is a name of one type, and every key that holds a name holds the
-- pair (type, name), directly or through a channel. Every existing row is duckdb_extension, the one
-- type there is (a grant without a name is any type, '*'), so every channel serves what it served and
-- every key holds what it held. An older replica's upserts would fail against the new keys, and it
-- must not read rows of a type it does not know, so it no longer runs here.
ALTER TABLE channels ADD COLUMN package_type varchar(32) NOT NULL DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE builds ADD COLUMN package_type varchar(32) NOT NULL DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE builds DROP CONSTRAINT ux_builds_source
-- +statement
ALTER TABLE builds ADD CONSTRAINT ux_builds_source UNIQUE (tenant_id, package_type, source, name, body_hash)
-- +statement
DROP INDEX ux_builds_slot
-- +statement
CREATE UNIQUE INDEX ux_builds_slot ON builds (tenant_id, package_type, source, name, package_version, platform, slot)
WHERE NOT legacy_duplicate AND source <> 'tenant'
-- +statement
DROP INDEX ix_builds_source_name
-- +statement
CREATE INDEX ix_builds_source_name ON builds (tenant_id, package_type, source, name, created_at)
-- +statement
DROP INDEX ix_builds_source_created
-- +statement
CREATE INDEX ix_builds_source_created ON builds (tenant_id, package_type, source, created_at)
-- +statement
-- a grant that names a package names its type; one without a name is any type ('*') or one type
ALTER TABLE grants ADD COLUMN package_type varchar(32) NULL
-- +statement
UPDATE grants SET package_type = CASE WHEN package IS NULL THEN '*' ELSE 'duckdb_extension' END
-- +statement
ALTER TABLE grants ALTER COLUMN package_type SET NOT NULL
-- +statement
ALTER TABLE grants ADD CONSTRAINT ck_grants_package_type CHECK (package IS NULL OR package_type <> '*')
-- +statement
ALTER TABLE package_group_members ADD COLUMN package_type varchar(32) NOT NULL DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE package_group_members DROP CONSTRAINT pk_package_group_members
-- +statement
ALTER TABLE package_group_members ADD CONSTRAINT pk_package_group_members PRIMARY KEY (group_id, package_type, package)
-- +statement
DROP INDEX ix_package_group_members_tenant
-- +statement
CREATE INDEX ix_package_group_members_tenant ON package_group_members (tenant_id, package_type, package)
-- +statement
-- the server's reserved names are per type: DuckDB's core and community names are duckdb_extension's
ALTER TABLE reserved_name_sources DROP CONSTRAINT reserved_name_sources_name_fkey
-- +statement
ALTER TABLE reserved_names ADD COLUMN package_type varchar(32) NOT NULL DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE reserved_names DROP CONSTRAINT reserved_names_pkey
-- +statement
ALTER TABLE reserved_names ADD CONSTRAINT pk_reserved_names PRIMARY KEY (package_type, name)
-- +statement
ALTER TABLE reserved_name_sources ADD COLUMN package_type varchar(32) NOT NULL DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE reserved_name_sources DROP CONSTRAINT reserved_name_sources_pkey
-- +statement
ALTER TABLE reserved_name_sources ADD CONSTRAINT pk_reserved_name_sources PRIMARY KEY (package_type, name, source)
-- +statement
ALTER TABLE reserved_name_sources ADD CONSTRAINT fk_reserved_name_sources_name FOREIGN KEY (package_type, name)
REFERENCES reserved_names (package_type, name)
-- +statement
ALTER TABLE tenant_reserved_names ADD COLUMN package_type varchar(32) NOT NULL DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE tenant_reserved_names DROP CONSTRAINT tenant_reserved_names_pkey
-- +statement
ALTER TABLE tenant_reserved_names ADD CONSTRAINT pk_tenant_reserved_names PRIMARY KEY (tenant_id, package_type, name)
-- +statement
ALTER TABLE licenses ADD COLUMN package_type varchar(32) NOT NULL DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE licenses DROP CONSTRAINT licenses_pkey
-- +statement
ALTER TABLE licenses ADD CONSTRAINT pk_licenses PRIMARY KEY (tenant_id, package_type, name)
