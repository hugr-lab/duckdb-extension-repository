-- spec 0012 phase 1a: a tenant's licences, one per extension name, verified against the server's
-- trusted keys when stored (the claims as columns, the token as received); a relayed licence names
-- its upstream, which removes it explicitly when it goes
CREATE TABLE licenses (
	tenant_id   TEXT NOT NULL REFERENCES tenants (id),
	name        TEXT NOT NULL,
	license_id  TEXT NOT NULL,
	issuer      TEXT NOT NULL,
	holder_id   TEXT NOT NULL,
	holder      TEXT NOT NULL,
	plan_name   TEXT NOT NULL,
	token       TEXT NOT NULL,
	key_fp      TEXT NOT NULL,
	issued_at   INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	grace_until INTEGER NOT NULL,
	source      TEXT NOT NULL CHECK (source IN ('import', 'upstream')),
	upstream_id TEXT NULL,
	visibility  TEXT NOT NULL CHECK (visibility IN ('private', 'public')),
	actor       TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	version     INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, name)
)
-- +statement
CREATE INDEX ix_licenses_upstream ON licenses (upstream_id)
-- +statement
-- each credentialed upstream's licence relay: a run requested, the last run and its result
CREATE TABLE license_syncs (
	upstream_id  TEXT PRIMARY KEY REFERENCES upstreams (id) ON DELETE CASCADE,
	tenant_id    TEXT NOT NULL REFERENCES tenants (id),
	requested_at TEXT NULL,
	last_run_at  TEXT NULL,
	last_result  TEXT NULL
)
