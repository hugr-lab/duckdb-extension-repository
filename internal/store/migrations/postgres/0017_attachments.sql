-- spec 0011: attachments, documents about a body in a tenant (1a); the statement's columns are here
-- so the feed (1b) needs no migration of this table
CREATE TABLE attachments (
	id            varchar(36) PRIMARY KEY,
	tenant_id     varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	body_hash     varchar(64) NOT NULL,
	kind          varchar(16) NOT NULL,
	media_type    varchar(100) NOT NULL,
	title         varchar(200) NOT NULL,
	size          bigint NOT NULL,
	content_hash  varchar(64) NOT NULL,
	summary       varchar(4000) NOT NULL,
	origin        varchar(16) NOT NULL,
	attached_by   varchar(400) NOT NULL,
	visibility    varchar(16) NOT NULL,
	feedable      boolean NOT NULL,
	version       bigint NOT NULL,
	removed_at    timestamptz,
	created_at    timestamptz NOT NULL,
	issuer        varchar(400) NOT NULL,
	issuer_id     varchar(64) NOT NULL,
	key_fp        varchar(80),
	source_origin varchar(80),
	issued_at     timestamptz NOT NULL,
	valid_until   timestamptz,
	supersedes    varchar(64),
	withdrawn_at  timestamptz,
	statement     text,
	signature     varchar(512),
	CONSTRAINT ck_attachments_kind CHECK (kind IN ('sbom', 'vulnerabilities', 'review', 'provenance', 'notes')),
	CONSTRAINT ck_attachments_origin CHECK (origin IN ('tenant', 'publisher', 'feed')),
	CONSTRAINT ck_attachments_visibility CHECK (visibility IN ('administrators', 'readers')),
	CONSTRAINT uq_attachments_issuer UNIQUE (tenant_id, issuer, issuer_id)
)
-- +statement
CREATE INDEX ix_attachments_body ON attachments (tenant_id, body_hash)
