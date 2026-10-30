-- +min_reader 48
-- spec 0039 PR 1: revocations downstream. A repository upstream whose remote is a kista (remote_kista)
-- reads the remote's revoked rows at each run (<prefix>/-/revocations, conditional on
-- revocations_etag) and stores those matching its own builds (upstream_revocations); switched on
-- (revocations), it applies them: a withdrawal or block as the upstream's block of the body
-- (upstream_blocks: the body refused from that upstream, its builds' releases yanked), a yank as a
-- yank in its home channel. What an upstream yanked is recorded (upstream_yanks) so a tenant
-- administrator's lift restores it. An older replica would take a body an upstream blocked from that
-- upstream again, and run a switched-on mirror without the pass, so it no longer runs here.
ALTER TABLE upstreams ADD COLUMN remote_kista boolean NOT NULL DEFAULT false
-- +statement
ALTER TABLE upstreams ADD COLUMN revocations boolean NOT NULL DEFAULT false
-- +statement
ALTER TABLE upstreams ADD COLUMN revocations_etag varchar(256) NULL
-- +statement
CREATE TABLE upstream_revocations (
	upstream_id     varchar(36) NOT NULL REFERENCES upstreams (id) ON DELETE NO ACTION,
	build_id        varchar(36) NOT NULL,
	tenant_id       varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	body_hash       varchar(64) NOT NULL,
	name            varchar(64) NOT NULL,
	package_version varchar(64) NOT NULL,
	platform        varchar(64) NOT NULL,
	state           varchar(16) NOT NULL,
	reason          varchar(400) NOT NULL,
	seen_at         timestamptz NOT NULL,
	applied_state   varchar(16) NULL,
	applied_at      timestamptz NULL,
	lifted_state    varchar(16) NULL,
	lifted_at       timestamptz NULL,
	unlisted_at     timestamptz NULL,
	PRIMARY KEY (upstream_id, build_id)
)
-- +statement
CREATE TABLE upstream_blocks (
	upstream_id varchar(36) NOT NULL REFERENCES upstreams (id) ON DELETE NO ACTION,
	body_hash   varchar(64) NOT NULL,
	tenant_id   varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	state       varchar(16) NOT NULL,
	reason      varchar(400) NOT NULL,
	created_at  timestamptz NOT NULL,
	PRIMARY KEY (upstream_id, body_hash)
)
-- +statement
CREATE TABLE upstream_yanks (
	release_id  varchar(36) NOT NULL PRIMARY KEY,
	tenant_id   varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	upstream_id varchar(36) NOT NULL,
	build_id    varchar(36) NOT NULL,
	body_hash   varchar(64) NOT NULL,
	prior_state varchar(16) NOT NULL,
	cause       varchar(8) NOT NULL
)
-- +statement
CREATE INDEX ix_upstream_yanks_build ON upstream_yanks (upstream_id, build_id)
-- +statement
CREATE INDEX ix_upstream_yanks_body ON upstream_yanks (tenant_id, body_hash)
-- +statement
CREATE INDEX ix_upstream_blocks_tenant ON upstream_blocks (tenant_id)
