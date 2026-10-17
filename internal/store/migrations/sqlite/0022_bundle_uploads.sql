-- +min_reader 22
-- spec 0014 phase 2: bundles uploaded over the API in chunks; an older replica would not import a
-- completed upload, and its collector would not know uploads/
CREATE TABLE bundle_uploads (
	id            TEXT PRIMARY KEY,
	tenant_id     TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	size          INTEGER NOT NULL,
	sha256        TEXT,
	chunk_size    INTEGER NOT NULL,
	options       text NOT NULL,
	state         TEXT NOT NULL CHECK (state IN ('uploading', 'queued', 'importing', 'imported', 'failed', 'aborted', 'expired')),
	import_id     TEXT,
	input_sha256  TEXT,
	attempts      INTEGER NOT NULL,
	error         TEXT,
	created_by    TEXT NOT NULL,
	completed_by  TEXT,
	created_at    TEXT NOT NULL,
	last_chunk_at TEXT,
	updated_at    TEXT NOT NULL
)
-- +statement
CREATE INDEX ix_bundle_uploads_state ON bundle_uploads (state, tenant_id)
-- +statement
CREATE TABLE bundle_upload_chunks (
	upload_id   TEXT NOT NULL REFERENCES bundle_uploads (id) ON DELETE CASCADE,
	n           INTEGER NOT NULL,
	object_key  TEXT NOT NULL,
	size        INTEGER NOT NULL,
	sha256      TEXT NOT NULL,
	received_at TEXT NOT NULL,
	PRIMARY KEY (upload_id, n)
)
-- +statement
CREATE INDEX ix_bundle_upload_chunks_key ON bundle_upload_chunks (object_key)
