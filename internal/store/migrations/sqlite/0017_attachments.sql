-- spec 0011: attachments, documents about a body in a tenant (1a); the statement's columns are here
-- so the feed (1b) needs no migration of this table
CREATE TABLE attachments (
	id            TEXT PRIMARY KEY,
	tenant_id     TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	body_hash     TEXT NOT NULL,
	kind          TEXT NOT NULL,
	media_type    TEXT NOT NULL,
	title         TEXT NOT NULL,
	size          INTEGER NOT NULL,
	content_hash  TEXT NOT NULL,
	summary       TEXT NOT NULL,
	origin        TEXT NOT NULL,
	attached_by   TEXT NOT NULL,
	visibility    TEXT NOT NULL,
	feedable      INTEGER NOT NULL,
	version       INTEGER NOT NULL,
	removed_at    TEXT,
	created_at    TEXT NOT NULL,
	issuer        TEXT NOT NULL,
	issuer_id     TEXT NOT NULL,
	key_fp        TEXT,
	source_origin TEXT,
	issued_at     TEXT NOT NULL,
	valid_until   TEXT,
	supersedes    TEXT,
	withdrawn_at  TEXT,
	statement     TEXT,
	signature     TEXT,
	CONSTRAINT ck_attachments_kind CHECK (kind IN ('sbom', 'vulnerabilities', 'review', 'provenance', 'notes')),
	CONSTRAINT ck_attachments_origin CHECK (origin IN ('tenant', 'publisher', 'feed')),
	CONSTRAINT ck_attachments_visibility CHECK (visibility IN ('administrators', 'readers')),
	CONSTRAINT uq_attachments_issuer UNIQUE (tenant_id, issuer, issuer_id)
)
-- +statement
CREATE INDEX ix_attachments_body ON attachments (tenant_id, body_hash)
