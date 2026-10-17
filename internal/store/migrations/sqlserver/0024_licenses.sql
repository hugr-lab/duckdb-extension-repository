-- spec 0012 phase 1a: a tenant's licences, one per extension name, verified against the server's
-- trusted keys when stored (the claims as columns, the token as received); a relayed licence names
-- its upstream, which removes it explicitly when it goes
CREATE TABLE licenses (
	tenant_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name        nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	license_id  nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	issuer      nvarchar(256) COLLATE Latin1_General_100_BIN2 NOT NULL,
	holder_id   nvarchar(128) COLLATE Latin1_General_100_BIN2 NOT NULL,
	holder      nvarchar(200) NOT NULL,
	plan_name   nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	token       nvarchar(max) NOT NULL,
	key_fp      nvarchar(71) COLLATE Latin1_General_100_BIN2 NOT NULL,
	issued_at   bigint NOT NULL,
	expires_at  bigint NOT NULL,
	grace_until bigint NOT NULL,
	source      nvarchar(8) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (source IN ('import', 'upstream')),
	upstream_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL,
	visibility  nvarchar(8) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (visibility IN ('private', 'public')),
	actor       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	updated_at  datetime2(6) NOT NULL,
	version     bigint NOT NULL,
	PRIMARY KEY (tenant_id, name)
)
-- +statement
CREATE INDEX ix_licenses_upstream ON licenses (upstream_id)
-- +statement
-- each credentialed upstream's licence relay: a run requested, the last run and its result
CREATE TABLE license_syncs (
	upstream_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY REFERENCES upstreams (id) ON DELETE CASCADE,
	tenant_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	requested_at datetime2(6) NULL,
	last_run_at  datetime2(6) NULL,
	last_result  nvarchar(max) NULL
)
