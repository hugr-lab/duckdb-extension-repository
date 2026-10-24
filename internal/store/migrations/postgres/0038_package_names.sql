-- +min_reader 38
-- spec 0034 phase 1: the schema says package where the thing is general (extension groups become
-- package groups, a grant's and a member's name a package, ext_version package_version), with no
-- change in what any row holds. An older replica would query the old names, so it no longer runs
-- here.
ALTER TABLE extension_groups RENAME TO package_groups
-- +statement
ALTER TABLE extension_group_members RENAME TO package_group_members
-- +statement
ALTER TABLE package_group_members RENAME COLUMN extension TO package
-- +statement
ALTER TABLE extension_group_grants RENAME TO package_group_grants
-- +statement
ALTER TABLE grants RENAME COLUMN extension TO package
-- +statement
ALTER TABLE builds RENAME COLUMN ext_version TO package_version
-- +statement
ALTER TABLE releases RENAME COLUMN ext_version TO package_version
-- +statement
ALTER TABLE purged_slots RENAME COLUMN ext_version TO package_version
-- +statement
ALTER TABLE upstream_versions RENAME COLUMN ext_version TO package_version
-- +statement
ALTER TABLE download_counts RENAME COLUMN ext_version TO package_version
-- +statement
ALTER TABLE download_installers RENAME COLUMN ext_version TO package_version
-- +statement
ALTER TABLE usage_counts RENAME COLUMN ext_version TO package_version
-- +statement
ALTER TABLE usage_users RENAME COLUMN ext_version TO package_version
-- +statement
ALTER TABLE usage_received RENAME COLUMN ext_version TO package_version
-- +statement
ALTER TABLE package_groups RENAME CONSTRAINT uq_extension_groups_name TO uq_package_groups_name
-- +statement
ALTER TABLE package_groups RENAME CONSTRAINT uq_extension_groups_tenant TO uq_package_groups_tenant
-- +statement
ALTER TABLE package_group_members RENAME CONSTRAINT pk_extension_group_members TO pk_package_group_members
-- +statement
ALTER TABLE package_group_members RENAME CONSTRAINT fk_extension_group_members_group TO fk_package_group_members_group
-- +statement
ALTER TABLE package_group_grants RENAME CONSTRAINT ck_extension_group_grants_kind TO ck_package_group_grants_kind
-- +statement
ALTER TABLE package_group_grants RENAME CONSTRAINT ck_extension_group_grants_principal TO ck_package_group_grants_principal
-- +statement
ALTER TABLE package_group_grants RENAME CONSTRAINT fk_extension_group_grants_group TO fk_package_group_grants_group
-- +statement
ALTER TABLE package_group_grants RENAME CONSTRAINT fk_extension_group_grants_issuer TO fk_package_group_grants_issuer
-- +statement
ALTER TABLE package_group_grants RENAME CONSTRAINT fk_extension_group_grants_publisher TO fk_package_group_grants_publisher
-- +statement
ALTER TABLE package_group_grants RENAME CONSTRAINT fk_extension_group_grants_channel TO fk_package_group_grants_channel
-- +statement
ALTER INDEX ix_extension_group_members_tenant RENAME TO ix_package_group_members_tenant
-- +statement
ALTER INDEX ix_extension_group_grants_tenant RENAME TO ix_package_group_grants_tenant
-- +statement
ALTER INDEX ix_extension_group_grants_group RENAME TO ix_package_group_grants_group
-- +statement
ALTER INDEX ix_extension_group_grants_issuer RENAME TO ix_package_group_grants_issuer
-- +statement
ALTER INDEX ix_extension_group_grants_publisher RENAME TO ix_package_group_grants_publisher
-- +statement
-- the system-named constraints follow too: the groups' and group grants' primary keys and the
-- groups' foreign key to tenants (and, on PostgreSQL 18, the not-null constraints, named after the
-- table and the column), with their indexes
DO $$
DECLARE r record;
BEGIN
	FOR r IN SELECT c.conrelid::regclass::text AS tbl, c.conname FROM pg_constraint c
		WHERE c.connamespace = (SELECT n.oid FROM pg_namespace n WHERE n.nspname = current_schema())
		AND (c.conname LIKE '%extension\_group%' OR c.conname LIKE '%ext\_version%')
	LOOP
		EXECUTE format('ALTER TABLE %s RENAME CONSTRAINT %I TO %I', r.tbl, r.conname,
			replace(replace(replace(r.conname, 'extension_group', 'package_group'), 'ext_version', 'package_version'),
				'_extension_not_null', '_package_not_null'));
	END LOOP;
END
$$
