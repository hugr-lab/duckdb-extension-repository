-- +min_reader 20
-- spec 0014: bundle-mode upstreams and bundle imports; an older replica would fetch a bundle-mode
-- upstream online, or give it a credential.
-- SQLite cannot change a CHECK: upstreams is rebuilt. Its five child tables cascade on delete, and
-- migrations run in a transaction with foreign keys on, so their rows are copied aside first and
-- restored after.
CREATE TABLE m20_upstream_keys AS SELECT * FROM upstream_keys
-- +statement
CREATE TABLE m20_upstream_platforms AS SELECT * FROM upstream_platforms
-- +statement
CREATE TABLE m20_upstream_entries AS SELECT * FROM upstream_entries
-- +statement
CREATE TABLE m20_upstream_versions AS SELECT * FROM upstream_versions
-- +statement
CREATE TABLE m20_upstream_cells AS SELECT * FROM upstream_cells
-- +statement
CREATE TABLE upstreams_new (
	id                     TEXT PRIMARY KEY,
	tenant_id              TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name                   TEXT NOT NULL,
	kind                   TEXT NOT NULL CHECK (kind IN ('duckdb-core', 'duckdb-community', 'repository')),
	prefix                 TEXT,
	channel_id             TEXT NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	mode                   TEXT NOT NULL CHECK (mode IN ('mirror', 'pull-through', 'bundle')),
	visibility             TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
	state                  TEXT NOT NULL CHECK (state IN ('active', 'paused')),
	version                INTEGER NOT NULL,
	requested_at           TEXT,
	request_dry_run        INTEGER NOT NULL,
	next_run_at            TEXT,
	last_run_at            TEXT,
	last_run               TEXT,
	created_at             TEXT NOT NULL,
	created_by             TEXT NOT NULL,
	credential             TEXT NULL,
	bundle_id              TEXT NULL,
	bundle_created_at      TEXT NULL,
	bundle_manifest_sha256 TEXT NULL,
	UNIQUE (tenant_id, name)
)
-- +statement
INSERT INTO upstreams_new (id, tenant_id, name, kind, prefix, channel_id, mode, visibility, state, version,
	requested_at, request_dry_run, next_run_at, last_run_at, last_run, created_at, created_by, credential)
SELECT id, tenant_id, name, kind, prefix, channel_id, mode, visibility, state, version,
	requested_at, request_dry_run, next_run_at, last_run_at, last_run, created_at, created_by, credential FROM upstreams
-- +statement
DROP TABLE upstreams
-- +statement
ALTER TABLE upstreams_new RENAME TO upstreams
-- +statement
CREATE INDEX ix_upstreams_channel ON upstreams (channel_id)
-- +statement
INSERT INTO upstream_keys SELECT * FROM m20_upstream_keys
-- +statement
INSERT INTO upstream_platforms SELECT * FROM m20_upstream_platforms
-- +statement
INSERT INTO upstream_entries SELECT * FROM m20_upstream_entries
-- +statement
INSERT INTO upstream_versions SELECT * FROM m20_upstream_versions
-- +statement
INSERT INTO upstream_cells SELECT * FROM m20_upstream_cells
-- +statement
DROP TABLE m20_upstream_keys
-- +statement
DROP TABLE m20_upstream_platforms
-- +statement
DROP TABLE m20_upstream_entries
-- +statement
DROP TABLE m20_upstream_versions
-- +statement
DROP TABLE m20_upstream_cells
-- +statement
CREATE TABLE bundle_imports (
	id                  TEXT PRIMARY KEY,
	tenant_id           TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	manifest_id         TEXT NOT NULL,
	manifest_created_at TEXT NOT NULL,
	manifest_sha256     TEXT NOT NULL,
	exporter            TEXT NOT NULL,
	bundle_key          TEXT NOT NULL,
	input_sha256        TEXT,
	dry_run             INTEGER NOT NULL,
	state               TEXT NOT NULL CHECK (state IN ('running', 'done', 'failed')),
	counts              TEXT NOT NULL,
	started_at          TEXT NOT NULL,
	finished_at         TEXT,
	actor               TEXT NOT NULL
)
-- +statement
CREATE INDEX ix_bundle_imports_tenant ON bundle_imports (tenant_id, started_at)
