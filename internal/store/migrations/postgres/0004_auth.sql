CREATE TABLE issuers (
	id                 varchar(36) PRIMARY KEY,
	tenant_id          varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name               varchar(16) NOT NULL,
	url                varchar(512) NOT NULL,
	jwks_uri           varchar(512),
	algorithms         varchar(200) NOT NULL,
	required_claims    varchar(4000) NOT NULL,
	roles_claim        varchar(400) NOT NULL,
	groups_claim       varchar(400) NOT NULL,
	client_claim       varchar(400) NOT NULL,
	max_token_lifetime bigint NOT NULL,
	created_at         timestamptz NOT NULL,
	created_by         varchar(400) NOT NULL,
	UNIQUE (tenant_id, name),
	UNIQUE (tenant_id, url),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE tenant_audiences (
	audience   varchar(400) PRIMARY KEY,
	tenant_id  varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	created_at timestamptz NOT NULL,
	created_by varchar(400) NOT NULL
)
-- +statement
CREATE INDEX ix_tenant_audiences_tenant ON tenant_audiences (tenant_id)
-- +statement
CREATE TABLE grants (
	id         varchar(36) PRIMARY KEY,
	tenant_id  varchar(36) NOT NULL,
	issuer_id  varchar(36) NOT NULL,
	kind       varchar(16) NOT NULL CHECK (kind IN ('subject', 'role', 'group', 'client', 'issuer')),
	value      varchar(256) NOT NULL,
	channel_id varchar(36),
	extension  varchar(64),
	verbs      varchar(100) NOT NULL,
	created_at timestamptz NOT NULL,
	created_by varchar(400) NOT NULL,
	FOREIGN KEY (issuer_id, tenant_id) REFERENCES issuers (id, tenant_id) ON DELETE NO ACTION,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION
)
-- +statement
CREATE INDEX ix_grants_tenant ON grants (tenant_id)
-- +statement
CREATE INDEX ix_grants_issuer ON grants (issuer_id)
-- +statement
ALTER TABLE tenants ADD COLUMN auth_version bigint NOT NULL DEFAULT 0
