-- +min_reader 48
-- spec 0039 PR 1: revocations downstream. A repository upstream whose remote is a kista (remote_kista)
-- reads the remote's revoked rows at each run (<prefix>/-/revocations, conditional on
-- revocations_etag) and stores those matching its own builds (upstream_revocations); switched on
-- (revocations), it applies them: a withdrawal or block as the upstream's block of the body
-- (upstream_blocks: the body refused from that upstream, its builds' releases yanked), a yank as a
-- yank in its home channel. What an upstream yanked is recorded (upstream_yanks) so a tenant
-- administrator's lift restores it. An older replica would take a body an upstream blocked from that
-- upstream again, and run a switched-on mirror without the pass, so it no longer runs here.
ALTER TABLE upstreams ADD remote_kista bit NOT NULL CONSTRAINT df_upstreams_remote_kista DEFAULT 0
-- +statement
ALTER TABLE upstreams ADD revocations bit NOT NULL CONSTRAINT df_upstreams_revocations DEFAULT 0
-- +statement
ALTER TABLE upstreams ADD revocations_etag nvarchar(256) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
CREATE TABLE upstream_revocations (
	upstream_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES upstreams (id) ON DELETE NO ACTION,
	build_id        nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	tenant_id       nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	body_hash       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	name            nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	package_version nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	platform        nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	state           nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	reason          nvarchar(400) NOT NULL,
	seen_at         datetime2(6) NOT NULL,
	applied_state   nvarchar(16) COLLATE Latin1_General_100_BIN2 NULL,
	applied_at      datetime2(6) NULL,
	lifted_state    nvarchar(16) COLLATE Latin1_General_100_BIN2 NULL,
	lifted_at       datetime2(6) NULL,
	unlisted_at     datetime2(6) NULL,
	PRIMARY KEY (upstream_id, build_id)
)
-- +statement
CREATE TABLE upstream_blocks (
	upstream_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES upstreams (id) ON DELETE NO ACTION,
	body_hash   nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	tenant_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	state       nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	reason      nvarchar(400) NOT NULL,
	created_at  datetime2(6) NOT NULL,
	PRIMARY KEY (upstream_id, body_hash)
)
-- +statement
CREATE TABLE upstream_yanks (
	release_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL PRIMARY KEY,
	tenant_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	upstream_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	build_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	body_hash   nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	prior_state nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	cause       nvarchar(8) COLLATE Latin1_General_100_BIN2 NOT NULL
)
-- +statement
CREATE INDEX ix_upstream_yanks_build ON upstream_yanks (upstream_id, build_id)
-- +statement
CREATE INDEX ix_upstream_yanks_body ON upstream_yanks (tenant_id, body_hash)
-- +statement
CREATE INDEX ix_upstream_blocks_tenant ON upstream_blocks (tenant_id)
