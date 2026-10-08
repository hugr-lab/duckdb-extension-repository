-- +min_reader 6
CREATE TABLE publishers (
	id         nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name       nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	created_at datetime2(6) NOT NULL,
	created_by nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	UNIQUE (tenant_id, name),
	UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE publisher_github (
	id            nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	publisher_id  nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES publishers (id) ON DELETE CASCADE,
	provider      nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	owner_id      nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	repository_id nvarchar(32) COLLATE Latin1_General_100_BIN2 NOT NULL,
	workflow      nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	ref           nvarchar(256) COLLATE Latin1_General_100_BIN2 NULL,
	environment   nvarchar(256) COLLATE Latin1_General_100_BIN2 NULL,
	created_at    datetime2(6) NOT NULL,
	created_by    nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL
)
-- +statement
CREATE INDEX ix_publisher_github_publisher ON publisher_github (publisher_id)
-- +statement
DROP INDEX ix_grants_issuer ON grants
-- +statement
DECLARE @n sysname = (SELECT name FROM sys.foreign_keys WHERE parent_object_id = OBJECT_ID('grants') AND referenced_object_id = OBJECT_ID('issuers'));
EXEC ('ALTER TABLE grants DROP CONSTRAINT ' + @n)
-- +statement
DECLARE @n sysname = (SELECT name FROM sys.check_constraints WHERE parent_object_id = OBJECT_ID('grants') AND definition LIKE '%subject%');
EXEC ('ALTER TABLE grants DROP CONSTRAINT ' + @n)
-- +statement
ALTER TABLE grants ALTER COLUMN issuer_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE grants ADD publisher_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL
-- +statement
ALTER TABLE grants ADD CONSTRAINT fk_grants_issuer FOREIGN KEY (issuer_id, tenant_id) REFERENCES issuers (id, tenant_id) ON DELETE NO ACTION
-- +statement
ALTER TABLE grants ADD CONSTRAINT fk_grants_publisher FOREIGN KEY (publisher_id, tenant_id) REFERENCES publishers (id, tenant_id) ON DELETE CASCADE
-- +statement
ALTER TABLE grants ADD CONSTRAINT ck_grants_kind CHECK (kind IN ('subject', 'role', 'group', 'client', 'issuer', 'publisher'))
-- +statement
ALTER TABLE grants ADD CONSTRAINT ck_grants_principal CHECK ((issuer_id IS NULL AND publisher_id IS NOT NULL AND kind = 'publisher')
	OR (issuer_id IS NOT NULL AND publisher_id IS NULL AND kind <> 'publisher'))
-- +statement
CREATE INDEX ix_grants_issuer ON grants (issuer_id)
-- +statement
CREATE INDEX ix_grants_publisher ON grants (publisher_id)
