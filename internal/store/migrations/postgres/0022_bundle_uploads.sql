-- +min_reader 22
-- spec 0014 phase 2: bundles uploaded over the API in chunks; an older replica would not import a
-- completed upload, and its collector would not know uploads/
CREATE TABLE bundle_uploads (
	id            varchar(36) PRIMARY KEY,
	tenant_id     varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	size          bigint NOT NULL,
	sha256        varchar(64),
	chunk_size    bigint NOT NULL,
	options       text NOT NULL,
	state         varchar(16) NOT NULL CHECK (state IN ('uploading', 'queued', 'importing', 'imported', 'failed', 'aborted', 'expired')),
	import_id     varchar(36),
	input_sha256  varchar(64),
	attempts      integer NOT NULL,
	error         varchar(200),
	created_by    varchar(400) NOT NULL,
	completed_by  varchar(400),
	created_at    timestamptz NOT NULL,
	last_chunk_at timestamptz,
	updated_at    timestamptz NOT NULL
)
-- +statement
CREATE INDEX ix_bundle_uploads_state ON bundle_uploads (state, tenant_id)
-- +statement
CREATE TABLE bundle_upload_chunks (
	upload_id   varchar(36) NOT NULL REFERENCES bundle_uploads (id) ON DELETE CASCADE,
	n           integer NOT NULL,
	object_key  varchar(80) NOT NULL,
	size        bigint NOT NULL,
	sha256      varchar(64) NOT NULL,
	received_at timestamptz NOT NULL,
	PRIMARY KEY (upload_id, n)
)
-- +statement
CREATE INDEX ix_bundle_upload_chunks_key ON bundle_upload_chunks (object_key)
