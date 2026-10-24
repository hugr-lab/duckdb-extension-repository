-- +min_reader 38
-- spec 0034 phase 1: the schema says package where the thing is general (extension groups become
-- package groups, a grant's and a member's name a package, ext_version package_version), with no
-- change in what any row holds. An older replica would query the old names, so it no longer runs
-- here. SQLite renames tables and columns in place (references and indexes follow); an index whose
-- name changes is dropped and created, and constraint names inside a table's definition, which
-- nothing addresses, stay as they were.
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
DROP INDEX ix_extension_group_members_tenant
-- +statement
CREATE INDEX ix_package_group_members_tenant ON package_group_members (tenant_id, package)
-- +statement
DROP INDEX ix_extension_group_grants_tenant
-- +statement
CREATE INDEX ix_package_group_grants_tenant ON package_group_grants (tenant_id)
-- +statement
DROP INDEX ix_extension_group_grants_group
-- +statement
CREATE INDEX ix_package_group_grants_group ON package_group_grants (group_id)
-- +statement
DROP INDEX ix_extension_group_grants_issuer
-- +statement
CREATE INDEX ix_package_group_grants_issuer ON package_group_grants (issuer_id)
-- +statement
DROP INDEX ix_extension_group_grants_publisher
-- +statement
CREATE INDEX ix_package_group_grants_publisher ON package_group_grants (publisher_id)
