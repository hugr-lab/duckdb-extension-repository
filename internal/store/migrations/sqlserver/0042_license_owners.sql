-- +min_reader 42
-- spec 0033 phase B part 2: licences per (name, source). In a tenant, each (type, name, key) has one
-- owner: the first source whose own entry recorded the key while it counted (available, or
-- released; not revoked; a distributor's publisher's not for a name a repository upstream of the
-- tenant lists). A licence's source is its key's owner for its name, and a channel serves the licence
-- of the source it selects for the name. The owners are the first recorders of today's records (one
-- recorded on a held entry is counted by the first claim after the hold); each licence gets its
-- key's owner, or 'unknown' when no record counts for its key (kept, served nowhere until imported
-- again). An older replica would serve a licence through any source's build and store licences by
-- name, so it no longer runs here. The primary key migration 0039 named is dropped by its name.
CREATE TABLE license_key_owners (
	tenant_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name         nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	kid          nvarchar(71) COLLATE Latin1_General_100_BIN2 NOT NULL,
	source       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	set_by       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	set_at       datetime2(6) NOT NULL,
	CONSTRAINT pk_license_key_owners PRIMARY KEY (tenant_id, package_type, name, kid)
)
-- +statement
CREATE INDEX ix_license_key_owners_source ON license_key_owners (source)
-- +statement
INSERT INTO license_key_owners (tenant_id, package_type, name, kid, source, set_by, set_at)
SELECT tenant_id, package_type, name, kid, source, 'system:licenses', recorded_at FROM (
	SELECT b.tenant_id, b.package_type, b.name, k.kid, k.source_kind + ':' + k.source_id AS source, k.recorded_at,
		ROW_NUMBER() OVER (PARTITION BY b.tenant_id, b.package_type, b.name, k.kid
			ORDER BY k.recorded_at, k.source_kind, k.source_id) AS n
	FROM build_license_keys k JOIN builds b ON b.id = k.build_id
	LEFT JOIN publishers p ON k.source_kind = 'publisher' AND p.id = k.source_id
	WHERE (k.source_kind = 'publisher' AND p.id IS NOT NULL
		OR k.source_kind = 'upstream' AND EXISTS (SELECT 1 FROM upstreams su WHERE su.id = k.source_id))
	AND b.source = k.source_kind + ':' + k.source_id AND b.revoked_at IS NULL
	AND (b.withdrawn_at IS NULL OR b.withdrawn_at > k.recorded_at)
	AND (b.available_at IS NULL OR b.available_at <= k.recorded_at OR EXISTS (SELECT 1 FROM releases r WHERE r.build_id = b.id))
	AND NOT (p.distributor IS NOT NULL AND EXISTS (SELECT 1 FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
		JOIN channels c ON c.id = u.channel_id
		WHERE u.tenant_id = b.tenant_id AND u.kind = 'repository' AND c.package_type = b.package_type AND (e.name = b.name OR e.name = '*')))
) o WHERE n = 1
-- +statement
ALTER TABLE licenses ADD owner_source nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
UPDATE licenses SET owner_source = COALESCE((SELECT o.source FROM license_key_owners o WHERE o.tenant_id = licenses.tenant_id
	AND o.package_type = licenses.package_type AND o.name = licenses.name AND o.kid = licenses.key_fp), 'unknown')
-- +statement
ALTER TABLE licenses ALTER COLUMN owner_source nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL
-- +statement
ALTER TABLE licenses DROP CONSTRAINT pk_licenses
-- +statement
ALTER TABLE licenses ADD CONSTRAINT pk_licenses PRIMARY KEY (tenant_id, package_type, name, owner_source)
