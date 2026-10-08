CREATE TABLE issuers (
	id                 TEXT PRIMARY KEY,
	tenant_id          TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name               TEXT NOT NULL,
	url                TEXT NOT NULL,
	jwks_uri           TEXT,
	algorithms         TEXT NOT NULL,
	required_claims    TEXT NOT NULL,
	roles_claim        TEXT NOT NULL,
	groups_claim       TEXT NOT NULL,
	client_claim       TEXT NOT NULL,
	max_token_lifetime INTEGER NOT NULL,
	created_at         TEXT NOT NULL,
	created_by         TEXT NOT NULL,
	UNIQUE (tenant_id, name),
	UNIQUE (tenant_id, url),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE tenant_audiences (
	audience   TEXT PRIMARY KEY,
	tenant_id  TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL
)
-- +statement
CREATE INDEX ix_tenant_audiences_tenant ON tenant_audiences (tenant_id)
-- +statement
CREATE TABLE grants (
	id         TEXT PRIMARY KEY,
	tenant_id  TEXT NOT NULL,
	issuer_id  TEXT NOT NULL,
	kind       TEXT NOT NULL CHECK (kind IN ('subject', 'role', 'group', 'client', 'issuer')),
	value      TEXT NOT NULL,
	channel_id TEXT,
	extension  TEXT,
	verbs      TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL,
	FOREIGN KEY (issuer_id, tenant_id) REFERENCES issuers (id, tenant_id) ON DELETE NO ACTION,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION
)
-- +statement
CREATE INDEX ix_grants_tenant ON grants (tenant_id)
-- +statement
CREATE INDEX ix_grants_issuer ON grants (issuer_id)
-- +statement
ALTER TABLE tenants ADD COLUMN auth_version INTEGER NOT NULL DEFAULT 0
