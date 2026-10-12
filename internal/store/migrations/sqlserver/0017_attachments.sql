-- spec 0011: attachments, documents about a body in a tenant (1a); the statement's columns are here
-- so the feed (1b) needs no migration of this table
CREATE TABLE attachments (
	id            nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	body_hash     nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	kind          nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	media_type    nvarchar(100) COLLATE Latin1_General_100_BIN2 NOT NULL,
	title         nvarchar(200) COLLATE Latin1_General_100_BIN2 NOT NULL,
	size          bigint NOT NULL,
	content_hash  nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	summary       nvarchar(4000) COLLATE Latin1_General_100_BIN2 NOT NULL,
	origin        nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	attached_by   nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	visibility    nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL,
	feedable      bit NOT NULL,
	version       bigint NOT NULL,
	removed_at    datetime2(6) NULL,
	created_at    datetime2(6) NOT NULL,
	issuer        nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	issuer_id     nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	key_fp        nvarchar(80) COLLATE Latin1_General_100_BIN2 NULL,
	source_origin nvarchar(80) COLLATE Latin1_General_100_BIN2 NULL,
	issued_at     datetime2(6) NOT NULL,
	valid_until   datetime2(6) NULL,
	supersedes    nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL,
	withdrawn_at  datetime2(6) NULL,
	statement     nvarchar(max) NULL,
	signature     nvarchar(512) COLLATE Latin1_General_100_BIN2 NULL,
	CONSTRAINT ck_attachments_kind CHECK (kind IN ('sbom', 'vulnerabilities', 'review', 'provenance', 'notes')),
	CONSTRAINT ck_attachments_origin CHECK (origin IN ('tenant', 'publisher', 'feed')),
	CONSTRAINT ck_attachments_visibility CHECK (visibility IN ('administrators', 'readers')),
	CONSTRAINT uq_attachments_issuer UNIQUE (tenant_id, issuer, issuer_id)
)
-- +statement
CREATE INDEX ix_attachments_body ON attachments (tenant_id, body_hash)
