-- spec 0012 phase 1a: a tenant's licences, one per extension name, verified against the server's
-- trusted keys when stored (the claims as columns, the token as received); a relayed licence names
-- its upstream, which removes it explicitly when it goes
CREATE TABLE licenses (
	tenant_id   varchar(36) NOT NULL REFERENCES tenants (id),
	name        varchar(64) NOT NULL,
	license_id  varchar(64) NOT NULL,
	issuer      varchar(256) NOT NULL,
	holder_id   varchar(128) NOT NULL,
	holder      varchar(200) NOT NULL,
	plan_name   varchar(32) NOT NULL,
	token       text NOT NULL,
	key_fp      varchar(71) NOT NULL,
	issued_at   bigint NOT NULL,
	expires_at  bigint NOT NULL,
	grace_until bigint NOT NULL,
	source      varchar(8) NOT NULL CHECK (source IN ('import', 'upstream')),
	upstream_id varchar(36) NULL,
	visibility  varchar(8) NOT NULL CHECK (visibility IN ('private', 'public')),
	actor       varchar(400) NOT NULL,
	updated_at  timestamptz NOT NULL,
	version     bigint NOT NULL,
	PRIMARY KEY (tenant_id, name)
)
-- +statement
CREATE INDEX ix_licenses_upstream ON licenses (upstream_id)
-- +statement
-- each credentialed upstream's licence relay: a run requested, the last run and its result
CREATE TABLE license_syncs (
	upstream_id  varchar(36) PRIMARY KEY REFERENCES upstreams (id) ON DELETE CASCADE,
	tenant_id    varchar(36) NOT NULL REFERENCES tenants (id),
	requested_at timestamptz NULL,
	last_run_at  timestamptz NULL,
	last_result  text NULL
)
