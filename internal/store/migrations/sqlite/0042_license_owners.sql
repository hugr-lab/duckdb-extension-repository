-- +min_reader 42
-- +foreign_keys_off
-- spec 0033 phase B part 2: licences per (name, source). In a tenant, each (type, name, key) has
-- one owner: the first source whose own entry recorded the key while it counted (from its
-- recording, the end of its hold, or its first release if sooner; not revoked, its body not
-- blocked, not withdrawn before; a distributor's publisher's not for a name a repository upstream
-- of the tenant lists). A licence's source is its key's owner for its name, and a channel serves
-- the licence of the source it selects for the name. The owners are the first recorders of today's
-- records, by the database's clock, as kista computes them (none while another source's record made
-- before is on an entry still held: the first claim after the hold counts it); each licence gets
-- its key's owner, or 'unknown' when there is none (kept, served nowhere until imported again). A
-- key's owner row whose source is '' is a removed owner's: only records made after its set_at count
-- toward the next owner. An older replica would serve a licence through any source's build and
-- store licences by name, so it no longer runs here. SQLite rebuilds licenses for its new key, with
-- foreign keys off and checked before the commit.
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
WITH recs AS (
	SELECT b.tenant_id, b.package_type, b.name, k.kid, k.source_kind || ':' || k.source_id AS source, k.recorded_at, b.withdrawn_at,
		CASE WHEN b.available_at IS NULL OR b.available_at <= k.recorded_at THEN k.recorded_at
			WHEN f.released < b.available_at THEN CASE WHEN f.released > k.recorded_at THEN f.released ELSE k.recorded_at END
			ELSE b.available_at END AS counts_at
	FROM build_license_keys k JOIN builds b ON b.id = k.build_id
	LEFT JOIN (SELECT build_id, MIN(created_at) AS released FROM releases GROUP BY build_id) f ON f.build_id = b.id
	LEFT JOIN publishers p ON k.source_kind = 'publisher' AND p.id = k.source_id
	WHERE (k.source_kind = 'publisher' AND p.id IS NOT NULL
		OR k.source_kind = 'upstream' AND EXISTS (SELECT 1 FROM upstreams su WHERE su.id = k.source_id))
	AND b.source = k.source_kind || ':' || k.source_id AND b.revoked_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash)
	AND NOT (p.distributor IS NOT NULL AND EXISTS (SELECT 1 FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
		JOIN channels c ON c.id = u.channel_id
		WHERE u.tenant_id = b.tenant_id AND u.kind = 'repository' AND c.package_type = b.package_type AND (e.name = b.name OR e.name = '*')))
), firsts AS (
	SELECT tenant_id, package_type, name, kid, source, counts_at,
		ROW_NUMBER() OVER (PARTITION BY tenant_id, package_type, name, kid ORDER BY counts_at, source) AS n
	FROM recs WHERE counts_at <= strftime('%Y-%m-%dT%H:%M:%f000Z', 'now') AND (withdrawn_at IS NULL OR withdrawn_at > counts_at)
)
INSERT INTO license_key_owners (tenant_id, package_type, name, kid, source, set_by, set_at)
SELECT f.tenant_id, f.package_type, f.name, f.kid, f.source, 'system:licenses', f.counts_at FROM firsts f WHERE f.n = 1
AND NOT EXISTS (SELECT 1 FROM recs r WHERE r.tenant_id = f.tenant_id AND r.package_type = f.package_type AND r.name = f.name AND r.kid = f.kid
	AND r.source <> f.source AND r.counts_at > strftime('%Y-%m-%dT%H:%M:%f000Z', 'now') AND r.withdrawn_at IS NULL AND r.recorded_at <= f.counts_at)
-- +statement
CREATE INDEX ix_build_license_keys_kid ON build_license_keys (tenant_id, kid)
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
