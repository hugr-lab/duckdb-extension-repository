-- +min_reader 6
CREATE TABLE publishers (
	id         varchar(36) PRIMARY KEY,
	tenant_id  varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name       varchar(64) NOT NULL,
	created_at timestamptz NOT NULL,
	created_by varchar(400) NOT NULL,
	UNIQUE (tenant_id, name),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE publisher_github (
	id            varchar(36) PRIMARY KEY,
	publisher_id  varchar(36) NOT NULL REFERENCES publishers (id) ON DELETE CASCADE,
	provider      varchar(64) NOT NULL,
	owner_id      varchar(32) NOT NULL,
	repository_id varchar(32) NOT NULL,
	workflow      varchar(400) NOT NULL,
	ref           varchar(256),
	environment   varchar(256),
	created_at    timestamptz NOT NULL,
	created_by    varchar(400) NOT NULL
)
-- +statement
CREATE INDEX ix_publisher_github_publisher ON publisher_github (publisher_id)
-- +statement
ALTER TABLE grants ALTER COLUMN issuer_id DROP NOT NULL
-- +statement
ALTER TABLE grants ADD COLUMN publisher_id varchar(36)
-- +statement
ALTER TABLE grants DROP CONSTRAINT grants_kind_check
-- +statement
ALTER TABLE grants ADD CONSTRAINT grants_kind_check CHECK (kind IN ('subject', 'role', 'group', 'client', 'issuer', 'publisher'))
-- +statement
ALTER TABLE grants ADD CONSTRAINT ck_grants_principal CHECK ((issuer_id IS NULL) <> (publisher_id IS NULL) AND (kind = 'publisher') = (publisher_id IS NOT NULL))
-- +statement
ALTER TABLE grants ADD CONSTRAINT fk_grants_publisher FOREIGN KEY (publisher_id, tenant_id) REFERENCES publishers (id, tenant_id) ON DELETE CASCADE
-- +statement
CREATE INDEX ix_grants_publisher ON grants (publisher_id)
