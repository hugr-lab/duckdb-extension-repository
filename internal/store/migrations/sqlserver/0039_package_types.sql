-- +min_reader 39
-- spec 0034 phase 2: a package is a name of one type, and every key that holds a name holds the
-- pair (type, name), directly or through a channel. Every existing row is duckdb_extension, the one
-- type there is (a grant without a name is any type, '*'), so every channel serves what it served and
-- every key holds what it held. An older replica must not read rows of a type it does not know, so it
-- no longer runs here. The system-named primary keys are found by their table and dropped.
ALTER TABLE channels ADD package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL
	CONSTRAINT df_channels_package_type DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE builds ADD package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL
	CONSTRAINT df_builds_package_type DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE builds DROP CONSTRAINT ux_builds_source
-- +statement
ALTER TABLE builds ADD CONSTRAINT ux_builds_source UNIQUE (tenant_id, package_type, source, name, body_hash)
-- +statement
DROP INDEX ux_builds_slot ON builds
-- +statement
CREATE UNIQUE INDEX ux_builds_slot ON builds (tenant_id, package_type, source, name, package_version, platform, slot)
WHERE legacy_duplicate = 0 AND source <> 'tenant'
-- +statement
DROP INDEX ix_builds_source_name ON builds
-- +statement
CREATE INDEX ix_builds_source_name ON builds (tenant_id, package_type, source, name, created_at)
-- +statement
DROP INDEX ix_builds_source_created ON builds
-- +statement
CREATE INDEX ix_builds_source_created ON builds (tenant_id, package_type, source, created_at)
-- +statement
-- a grant that names a package names its type; one without a name is any type ('*') or one type
ALTER TABLE grants ADD package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
UPDATE grants SET package_type = CASE WHEN package IS NULL THEN '*' ELSE 'duckdb_extension' END
-- +statement
ALTER TABLE grants ALTER COLUMN package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL
-- +statement
ALTER TABLE grants ADD CONSTRAINT ck_grants_package_type CHECK (package IS NULL OR package_type <> '*')
-- +statement
ALTER TABLE package_group_members ADD package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL
	CONSTRAINT df_package_group_members_package_type DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE package_group_members DROP CONSTRAINT pk_package_group_members
-- +statement
ALTER TABLE package_group_members ADD CONSTRAINT pk_package_group_members PRIMARY KEY (group_id, package_type, package)
-- +statement
DROP INDEX ix_package_group_members_tenant ON package_group_members
-- +statement
CREATE INDEX ix_package_group_members_tenant ON package_group_members (tenant_id, package_type, package)
-- +statement
-- the server's reserved names are per type: DuckDB's core and community names are duckdb_extension's
ALTER TABLE reserved_name_sources DROP CONSTRAINT fk_reserved_name_sources_name
-- +statement
ALTER TABLE reserved_names ADD package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL
	CONSTRAINT df_reserved_names_package_type DEFAULT 'duckdb_extension'
-- +statement
DECLARE @n sysname = (SELECT name FROM sys.key_constraints WHERE parent_object_id = OBJECT_ID('reserved_names') AND type = 'PK');
IF @n IS NULL THROW 50000, 'migration 0039: the reserved_names primary key was not found', 1;
DECLARE @q nvarchar(300) = N'ALTER TABLE reserved_names DROP CONSTRAINT ' + QUOTENAME(@n);
EXEC (@q)
-- +statement
ALTER TABLE reserved_names ADD CONSTRAINT pk_reserved_names PRIMARY KEY (package_type, name)
-- +statement
ALTER TABLE reserved_name_sources ADD package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL
	CONSTRAINT df_reserved_name_sources_package_type DEFAULT 'duckdb_extension'
-- +statement
ALTER TABLE reserved_name_sources DROP CONSTRAINT pk_reserved_name_sources
-- +statement
-- nonclustered: (type, name, source) is over the 900 bytes a clustered key takes (an administrator's
-- source is up to 386 characters), as feed_kept's issuer key
ALTER TABLE reserved_name_sources ADD CONSTRAINT pk_reserved_name_sources PRIMARY KEY NONCLUSTERED (package_type, name, source)
-- +statement
ALTER TABLE reserved_name_sources ADD CONSTRAINT fk_reserved_name_sources_name FOREIGN KEY (package_type, name)
REFERENCES reserved_names (package_type, name)
-- +statement
ALTER TABLE tenant_reserved_names ADD package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL
	CONSTRAINT df_tenant_reserved_names_package_type DEFAULT 'duckdb_extension'
-- +statement
DECLARE @n sysname = (SELECT name FROM sys.key_constraints WHERE parent_object_id = OBJECT_ID('tenant_reserved_names') AND type = 'PK');
IF @n IS NULL THROW 50000, 'migration 0039: the tenant_reserved_names primary key was not found', 1;
DECLARE @q nvarchar(300) = N'ALTER TABLE tenant_reserved_names DROP CONSTRAINT ' + QUOTENAME(@n);
EXEC (@q)
-- +statement
ALTER TABLE tenant_reserved_names ADD CONSTRAINT pk_tenant_reserved_names PRIMARY KEY (tenant_id, package_type, name)
-- +statement
ALTER TABLE licenses ADD package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL
	CONSTRAINT df_licenses_package_type DEFAULT 'duckdb_extension'
-- +statement
DECLARE @n sysname = (SELECT name FROM sys.key_constraints WHERE parent_object_id = OBJECT_ID('licenses') AND type = 'PK');
IF @n IS NULL THROW 50000, 'migration 0039: the licenses primary key was not found', 1;
DECLARE @q nvarchar(300) = N'ALTER TABLE licenses DROP CONSTRAINT ' + QUOTENAME(@n);
EXEC (@q)
-- +statement
ALTER TABLE licenses ADD CONSTRAINT pk_licenses PRIMARY KEY (tenant_id, package_type, name)
