-- +min_reader 42
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
-- store licences by name, so it no longer runs here. The primary key migration 0039 named is
-- dropped by its name.
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
WITH recs AS (
	SELECT b.tenant_id, b.package_type, b.name, k.kid, k.source_kind + ':' + k.source_id AS source, k.recorded_at, b.withdrawn_at,
		CASE WHEN b.available_at IS NULL OR b.available_at <= k.recorded_at THEN k.recorded_at
			WHEN f.released < b.available_at THEN CASE WHEN f.released > k.recorded_at THEN f.released ELSE k.recorded_at END
			ELSE b.available_at END AS counts_at
	FROM build_license_keys k JOIN builds b ON b.id = k.build_id
	LEFT JOIN (SELECT build_id, MIN(created_at) AS released FROM releases GROUP BY build_id) f ON f.build_id = b.id
	LEFT JOIN publishers p ON k.source_kind = 'publisher' AND p.id = k.source_id
	WHERE (k.source_kind = 'publisher' AND p.id IS NOT NULL
		OR k.source_kind = 'upstream' AND EXISTS (SELECT 1 FROM upstreams su WHERE su.id = k.source_id))
	AND b.source = k.source_kind + ':' + k.source_id AND b.revoked_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = b.tenant_id AND x.body_hash = b.body_hash)
	AND NOT (p.distributor IS NOT NULL AND EXISTS (SELECT 1 FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
		JOIN channels c ON c.id = u.channel_id
		WHERE u.tenant_id = b.tenant_id AND u.kind = 'repository' AND c.package_type = b.package_type AND (e.name = b.name OR e.name = '*')))
), firsts AS (
	SELECT tenant_id, package_type, name, kid, source, counts_at,
		ROW_NUMBER() OVER (PARTITION BY tenant_id, package_type, name, kid ORDER BY counts_at, source) AS n
	FROM recs WHERE counts_at <= SYSUTCDATETIME() AND (withdrawn_at IS NULL OR withdrawn_at > counts_at)
)
INSERT INTO license_key_owners (tenant_id, package_type, name, kid, source, set_by, set_at)
SELECT f.tenant_id, f.package_type, f.name, f.kid, f.source, 'system:licenses', f.counts_at FROM firsts f WHERE f.n = 1
AND NOT EXISTS (SELECT 1 FROM recs r WHERE r.tenant_id = f.tenant_id AND r.package_type = f.package_type AND r.name = f.name AND r.kid = f.kid
	AND r.source <> f.source AND r.counts_at > SYSUTCDATETIME() AND r.withdrawn_at IS NULL AND r.recorded_at <= f.counts_at)
-- +statement
CREATE INDEX ix_build_license_keys_kid ON build_license_keys (tenant_id, kid)
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
