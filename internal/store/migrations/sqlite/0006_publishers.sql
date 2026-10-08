-- +min_reader 6
CREATE TABLE publishers (
	id         TEXT PRIMARY KEY,
	tenant_id  TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL,
	UNIQUE (tenant_id, name),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE publisher_github (
	id            TEXT PRIMARY KEY,
	publisher_id  TEXT NOT NULL REFERENCES publishers (id) ON DELETE CASCADE,
	provider      TEXT NOT NULL,
	owner_id      TEXT NOT NULL,
	repository_id TEXT NOT NULL,
	workflow      TEXT NOT NULL,
	ref           TEXT,
	environment   TEXT,
	created_at    TEXT NOT NULL,
	created_by    TEXT NOT NULL
)
-- +statement
CREATE INDEX ix_publisher_github_publisher ON publisher_github (publisher_id)
-- +statement
CREATE TABLE grants_new (
	id           TEXT PRIMARY KEY,
	tenant_id    TEXT NOT NULL,
	issuer_id    TEXT,
	publisher_id TEXT,
	kind         TEXT NOT NULL CHECK (kind IN ('subject', 'role', 'group', 'client', 'issuer', 'publisher')),
	value        TEXT NOT NULL,
	channel_id   TEXT,
	extension    TEXT,
	verbs        TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	created_by   TEXT NOT NULL,
	CHECK ((issuer_id IS NULL) <> (publisher_id IS NULL)),
	CHECK ((kind = 'publisher') = (publisher_id IS NOT NULL)),
	FOREIGN KEY (issuer_id, tenant_id) REFERENCES issuers (id, tenant_id) ON DELETE NO ACTION,
	FOREIGN KEY (publisher_id, tenant_id) REFERENCES publishers (id, tenant_id) ON DELETE CASCADE,
	FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION
)
-- +statement
INSERT INTO grants_new (id, tenant_id, issuer_id, kind, value, channel_id, extension, verbs, created_at, created_by)
SELECT id, tenant_id, issuer_id, kind, value, channel_id, extension, verbs, created_at, created_by FROM grants
-- +statement
DROP TABLE grants
-- +statement
ALTER TABLE grants_new RENAME TO grants
-- +statement
CREATE INDEX ix_grants_tenant ON grants (tenant_id)
-- +statement
CREATE INDEX ix_grants_issuer ON grants (issuer_id)
-- +statement
CREATE INDEX ix_grants_publisher ON grants (publisher_id)
