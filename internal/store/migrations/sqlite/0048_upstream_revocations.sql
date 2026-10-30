-- +min_reader 48
-- spec 0039 PR 1: revocations downstream. A repository upstream whose remote is a kista (remote_kista)
-- reads the remote's revoked rows at each run (<prefix>/-/revocations, conditional on
-- revocations_etag) and stores those matching its own builds (upstream_revocations); switched on
-- (revocations), it applies them: a withdrawal or block as the upstream's block of the body
-- (upstream_blocks: the body refused from that upstream, its builds' releases yanked), a yank as a
-- yank in its home channel. What an upstream yanked is recorded (upstream_yanks) so a tenant
-- administrator's lift restores it. An older replica would take a body an upstream blocked from that
-- upstream again, and run a switched-on mirror without the pass, so it no longer runs here.
ALTER TABLE upstreams ADD COLUMN remote_kista INTEGER NOT NULL DEFAULT 0
-- +statement
ALTER TABLE upstreams ADD COLUMN revocations INTEGER NOT NULL DEFAULT 0
-- +statement
ALTER TABLE upstreams ADD COLUMN revocations_etag TEXT NULL
-- +statement
CREATE TABLE upstream_revocations (
	upstream_id     TEXT NOT NULL REFERENCES upstreams (id) ON DELETE NO ACTION,
	build_id        TEXT NOT NULL,
	tenant_id       TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	body_hash       TEXT NOT NULL,
	name            TEXT NOT NULL,
	package_version TEXT NOT NULL,
	platform        TEXT NOT NULL,
	state           TEXT NOT NULL,
	reason          TEXT NOT NULL,
	seen_at         TEXT NOT NULL,
	applied_state   TEXT NULL,
	applied_at      TEXT NULL,
	lifted_state    TEXT NULL,
	lifted_at       TEXT NULL,
	unlisted_at     TEXT NULL,
	PRIMARY KEY (upstream_id, build_id)
)
-- +statement
CREATE TABLE upstream_blocks (
	upstream_id TEXT NOT NULL REFERENCES upstreams (id) ON DELETE NO ACTION,
	body_hash   TEXT NOT NULL,
	tenant_id   TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	state       TEXT NOT NULL,
	reason      TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	PRIMARY KEY (upstream_id, body_hash)
)
-- +statement
CREATE TABLE upstream_yanks (
	release_id  TEXT NOT NULL PRIMARY KEY,
	tenant_id   TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	upstream_id TEXT NOT NULL,
	build_id    TEXT NOT NULL,
	body_hash   TEXT NOT NULL,
	prior_state TEXT NOT NULL,
	cause       TEXT NOT NULL
)
-- +statement
CREATE INDEX ix_upstream_yanks_build ON upstream_yanks (upstream_id, build_id)
-- +statement
CREATE INDEX ix_upstream_yanks_body ON upstream_yanks (tenant_id, body_hash)
-- +statement
CREATE INDEX ix_upstream_blocks_tenant ON upstream_blocks (tenant_id)
