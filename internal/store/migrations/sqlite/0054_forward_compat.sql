-- +foreign_keys_off
-- spec 0046: forward compatibility. A feed's cursor remembers what it passed over because this
-- binary could not read it (a newer statement format, a kind, media type or document version it does
-- not know): the lowest such seq, how many, the last reason, and the reader level of the binary that
-- passed over them, so a binary of a higher level takes them again. An attachment's kinds are
-- validated in the code only (internal/attachment): a row of any kind is read as stored, so a later
-- release that adds a kind needs no migration. An older replica ignores the new columns and inserts a
-- cursor with their defaults: min_reader is unchanged. An attachment gains narrowed: a statement in a
-- newer format said it is not for readers, which holds until a readable newer statement arrives.
-- SQLite rebuilds attachments to drop its kind's
-- CHECK, with foreign keys off and checked before the commit, keeping its index.
ALTER TABLE feed_cursors ADD COLUMN waiting_from INTEGER NULL
-- +statement
ALTER TABLE feed_cursors ADD COLUMN waiting INTEGER NOT NULL DEFAULT 0
-- +statement
ALTER TABLE feed_cursors ADD COLUMN waiting_why TEXT NULL
-- +statement
ALTER TABLE feed_cursors ADD COLUMN reader_level INTEGER NOT NULL DEFAULT 0
-- +statement
CREATE TABLE attachments_new (
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
	seq           INTEGER,
	narrowed      INTEGER NOT NULL DEFAULT 0,
	CONSTRAINT ck_attachments_origin CHECK (origin IN ('tenant', 'publisher', 'feed')),
	CONSTRAINT ck_attachments_visibility CHECK (visibility IN ('administrators', 'readers')),
	CONSTRAINT uq_attachments_issuer UNIQUE (tenant_id, issuer, issuer_id)
)
-- +statement
INSERT INTO attachments_new (id, tenant_id, body_hash, kind, media_type, title, size, content_hash, summary, origin, attached_by,
	visibility, feedable, version, removed_at, created_at, issuer, issuer_id, key_fp, source_origin, issued_at, valid_until, supersedes,
	withdrawn_at, statement, signature, seq)
SELECT id, tenant_id, body_hash, kind, media_type, title, size, content_hash, summary, origin, attached_by, visibility, feedable,
	version, removed_at, created_at, issuer, issuer_id, key_fp, source_origin, issued_at, valid_until, supersedes, withdrawn_at,
	statement, signature, seq
FROM attachments
-- +statement
DROP TABLE attachments
-- +statement
ALTER TABLE attachments_new RENAME TO attachments
-- +statement
CREATE INDEX ix_attachments_body ON attachments (tenant_id, body_hash)
