CREATE TABLE issuers (
	id                 nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id          nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name               nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	url                nvarchar(512) COLLATE Latin1_General_100_BIN2 NOT NULL,
	jwks_uri           nvarchar(512) COLLATE Latin1_General_100_BIN2,
	algorithms         nvarchar(200) COLLATE Latin1_General_100_BIN2 NOT NULL,
	required_claims    nvarchar(4000) COLLATE Latin1_General_100_BIN2 NOT NULL,
	roles_claim        nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	groups_claim       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	client_claim       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	max_token_lifetime bigint NOT NULL,
	created_at         datetime2(6) NOT NULL,
	created_by         nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	UNIQUE (tenant_id, name),
	UNIQUE (tenant_id, url),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE tenant_audiences (
	audience   nvarchar(400) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	created_at datetime2(6) NOT NULL,
	created_by nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL
)
-- +statement
CREATE INDEX ix_tenant_audiences_tenant ON tenant_audiences (tenant_id)
-- +statement
CREATE TABLE grants (
	id         nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	issuer_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL,
	kind       nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (kind IN ('subject', 'role', 'group', 'client', 'issuer')),
	value      nvarchar(256) COLLATE Latin1_General_100_BIN2 NOT NULL,
	channel_id nvarchar(36) COLLATE Latin1_General_100_BIN2,
	extension  nvarchar(64) COLLATE Latin1_General_100_BIN2,
	verbs      nvarchar(100) COLLATE Latin1_General_100_BIN2 NOT NULL,
	created_at datetime2(6) NOT NULL,
	created_by nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	FOREIGN KEY (issuer_id, tenant_id) REFERENCES issuers (id, tenant_id) ON DELETE NO ACTION,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION
)
-- +statement
CREATE INDEX ix_grants_tenant ON grants (tenant_id)
-- +statement
CREATE INDEX ix_grants_issuer ON grants (issuer_id)
-- +statement
ALTER TABLE tenants ADD auth_version bigint NOT NULL CONSTRAINT df_tenants_auth_version DEFAULT 0
