-- +min_reader 22
-- spec 0014 phase 2: bundles uploaded over the API in chunks; an older replica would not import a
-- completed upload, and its collector would not know uploads/
CREATE TABLE bundle_uploads (
	id            nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	size          bigint NOT NULL,
	sha256        nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL,
	chunk_size    bigint NOT NULL,
	options       nvarchar(max) NOT NULL,
	state         nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('uploading', 'queued', 'importing', 'imported', 'failed', 'aborted', 'expired')),
	import_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NULL,
	input_sha256  nvarchar(64) COLLATE Latin1_General_100_BIN2 NULL,
	attempts      integer NOT NULL,
	error         nvarchar(200) COLLATE Latin1_General_100_BIN2 NULL,
	created_by    nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	completed_by  nvarchar(400) COLLATE Latin1_General_100_BIN2 NULL,
	created_at    datetime2(6) NOT NULL,
	last_chunk_at datetime2(6) NULL,
	updated_at    datetime2(6) NOT NULL
)
-- +statement
CREATE INDEX ix_bundle_uploads_state ON bundle_uploads (state, tenant_id)
-- +statement
CREATE TABLE bundle_upload_chunks (
	upload_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES bundle_uploads (id) ON DELETE CASCADE,
	n           integer NOT NULL,
	object_key  nvarchar(80) COLLATE Latin1_General_100_BIN2 NOT NULL,
	size        bigint NOT NULL,
	sha256      nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	received_at datetime2(6) NOT NULL,
	PRIMARY KEY (upload_id, n)
)
-- +statement
CREATE INDEX ix_bundle_upload_chunks_key ON bundle_upload_chunks (object_key)
