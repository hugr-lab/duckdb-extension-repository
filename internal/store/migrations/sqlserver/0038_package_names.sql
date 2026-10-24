-- +min_reader 38
-- spec 0034 phase 1: the schema says package where the thing is general (extension groups become
-- package groups, a grant's and a member's name a package, ext_version package_version), with no
-- change in what any row holds. An older replica would query the old names, so it no longer runs
-- here. The system-named primary keys and foreign key of the group tables (PK__extensio__…) keep
-- their generated names.
EXEC sp_rename N'extension_groups', N'package_groups'
-- +statement
EXEC sp_rename N'extension_group_members', N'package_group_members'
-- +statement
EXEC sp_rename N'package_group_members.extension', N'package', N'COLUMN'
-- +statement
EXEC sp_rename N'extension_group_grants', N'package_group_grants'
-- +statement
EXEC sp_rename N'grants.extension', N'package', N'COLUMN'
-- +statement
EXEC sp_rename N'builds.ext_version', N'package_version', N'COLUMN'
-- +statement
EXEC sp_rename N'releases.ext_version', N'package_version', N'COLUMN'
-- +statement
EXEC sp_rename N'purged_slots.ext_version', N'package_version', N'COLUMN'
-- +statement
EXEC sp_rename N'upstream_versions.ext_version', N'package_version', N'COLUMN'
-- +statement
EXEC sp_rename N'download_counts.ext_version', N'package_version', N'COLUMN'
-- +statement
EXEC sp_rename N'download_installers.ext_version', N'package_version', N'COLUMN'
-- +statement
EXEC sp_rename N'usage_counts.ext_version', N'package_version', N'COLUMN'
-- +statement
EXEC sp_rename N'usage_users.ext_version', N'package_version', N'COLUMN'
-- +statement
EXEC sp_rename N'usage_received.ext_version', N'package_version', N'COLUMN'
-- +statement
EXEC sp_rename N'uq_extension_groups_name', N'uq_package_groups_name', N'OBJECT'
-- +statement
EXEC sp_rename N'uq_extension_groups_tenant', N'uq_package_groups_tenant', N'OBJECT'
-- +statement
EXEC sp_rename N'pk_extension_group_members', N'pk_package_group_members', N'OBJECT'
-- +statement
EXEC sp_rename N'fk_extension_group_members_group', N'fk_package_group_members_group', N'OBJECT'
-- +statement
EXEC sp_rename N'ck_extension_group_grants_kind', N'ck_package_group_grants_kind', N'OBJECT'
-- +statement
EXEC sp_rename N'ck_extension_group_grants_principal', N'ck_package_group_grants_principal', N'OBJECT'
-- +statement
EXEC sp_rename N'fk_extension_group_grants_group', N'fk_package_group_grants_group', N'OBJECT'
-- +statement
EXEC sp_rename N'fk_extension_group_grants_issuer', N'fk_package_group_grants_issuer', N'OBJECT'
-- +statement
EXEC sp_rename N'fk_extension_group_grants_publisher', N'fk_package_group_grants_publisher', N'OBJECT'
-- +statement
EXEC sp_rename N'fk_extension_group_grants_channel', N'fk_package_group_grants_channel', N'OBJECT'
-- +statement
EXEC sp_rename N'package_group_members.ix_extension_group_members_tenant', N'ix_package_group_members_tenant', N'INDEX'
-- +statement
EXEC sp_rename N'package_group_grants.ix_extension_group_grants_tenant', N'ix_package_group_grants_tenant', N'INDEX'
-- +statement
EXEC sp_rename N'package_group_grants.ix_extension_group_grants_group', N'ix_package_group_grants_group', N'INDEX'
-- +statement
EXEC sp_rename N'package_group_grants.ix_extension_group_grants_issuer', N'ix_package_group_grants_issuer', N'INDEX'
-- +statement
EXEC sp_rename N'package_group_grants.ix_extension_group_grants_publisher', N'ix_package_group_grants_publisher', N'INDEX'
