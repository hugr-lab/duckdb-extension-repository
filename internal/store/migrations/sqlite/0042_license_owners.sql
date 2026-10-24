-- +min_reader 42
-- +foreign_keys_off
-- spec 0033 phase B part 2: licences per (name, source). In a tenant, each (type, name, key) has one
-- owner: the first source whose own entry recorded the key while it counted (available, or
-- released; not revoked; a distributor's publisher's not for a name a repository upstream of the
-- tenant lists). A licence's source is its key's owner for its name, and a channel serves the licence
-- of the source it selects for the name. The owners are the first recorders of today's records (one
-- recorded on a held entry is counted by the first claim after the hold); each licence gets its
-- key's owner, or 'unknown' when no record counts for its key (kept, served nowhere until imported
-- again). An older replica would serve a licence through any source's build and store licences by
-- name, so it no longer runs here. SQLite rebuilds licenses for its new key, with foreign keys off
-- and checked before the commit.
CREATE TABLE license_key_owners (
	tenant_id    TEXT NOT NULL REFERENCES tenants (id),
	package_type TEXT NOT NULL,
	name         TEXT NOT NULL,
	kid          TEXT NOT NULL,
	source       TEXT NOT NULL,
	set_by       TEXT NOT NULL,
	set_at       TEXT NOT NULL,
	CONSTRAINT pk_license_key_owners PRIMARY KEY (tenant_id, package_type, name, kid)
)
-- +statement
CREATE INDEX ix_license_key_owners_source ON license_key_owners (source)
-- +statement
INSERT INTO license_key_owners (tenant_id, package_type, name, kid, source, set_by, set_at)
SELECT tenant_id, package_type, name, kid, source, 'system:licenses', recorded_at FROM (
	SELECT b.tenant_id, b.package_type, b.name, k.kid, k.source_kind || ':' || k.source_id AS source, k.recorded_at,
		ROW_NUMBER() OVER (PARTITION BY b.tenant_id, b.package_type, b.name, k.kid
			ORDER BY k.recorded_at, k.source_kind, k.source_id) AS n
	FROM build_license_keys k JOIN builds b ON b.id = k.build_id
	LEFT JOIN publishers p ON k.source_kind = 'publisher' AND p.id = k.source_id
	WHERE (k.source_kind = 'publisher' AND p.id IS NOT NULL
		OR k.source_kind = 'upstream' AND EXISTS (SELECT 1 FROM upstreams su WHERE su.id = k.source_id))
	AND b.source = k.source_kind || ':' || k.source_id AND b.revoked_at IS NULL
	AND (b.withdrawn_at IS NULL OR b.withdrawn_at > k.recorded_at)
	AND (b.available_at IS NULL OR b.available_at <= k.recorded_at OR EXISTS (SELECT 1 FROM releases r WHERE r.build_id = b.id))
	AND NOT (p.distributor IS NOT NULL AND EXISTS (SELECT 1 FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
		JOIN channels c ON c.id = u.channel_id
		WHERE u.tenant_id = b.tenant_id AND u.kind = 'repository' AND c.package_type = b.package_type AND (e.name = b.name OR e.name = '*')))
) o WHERE n = 1
-- +statement
CREATE TABLE licenses_new (
	tenant_id    TEXT NOT NULL REFERENCES tenants (id),
	package_type TEXT NOT NULL DEFAULT 'duckdb_extension',
	name         TEXT NOT NULL,
	owner_source TEXT NOT NULL,
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
	CONSTRAINT pk_licenses PRIMARY KEY (tenant_id, package_type, name, owner_source)
)
-- +statement
INSERT INTO licenses_new (tenant_id, package_type, name, owner_source, license_id, issuer, holder_id, holder, plan_name, token, key_fp,
	issued_at, expires_at, grace_until, source, upstream_id, visibility, actor, updated_at, version)
SELECT l.tenant_id, l.package_type, l.name, COALESCE((SELECT o.source FROM license_key_owners o WHERE o.tenant_id = l.tenant_id
	AND o.package_type = l.package_type AND o.name = l.name AND o.kid = l.key_fp), 'unknown'), l.license_id, l.issuer, l.holder_id,
	l.holder, l.plan_name, l.token, l.key_fp, l.issued_at, l.expires_at, l.grace_until, l.source, l.upstream_id, l.visibility, l.actor,
	l.updated_at, l.version
FROM licenses l
-- +statement
DROP TABLE licenses
-- +statement
ALTER TABLE licenses_new RENAME TO licenses
-- +statement
CREATE INDEX ix_licenses_upstream ON licenses (upstream_id)
