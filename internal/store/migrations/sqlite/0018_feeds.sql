-- +min_reader 18
-- spec 0011 phase 1b: attestation keys, the statements a tenant issues, and the feeds it subscribes
-- to; an older replica would mark documents feedable without issuing their statements
CREATE TABLE attestation_keys (
	id               TEXT PRIMARY KEY,
	tenant_id        TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	fingerprint      TEXT NOT NULL UNIQUE REFERENCES key_fingerprints (fingerprint) ON DELETE NO ACTION,
	signer_ref       TEXT NOT NULL,
	public_key       BLOB NOT NULL,
	state            TEXT NOT NULL CHECK (state IN ('trusted', 'active', 'retired')),
	trusted_since    TEXT NOT NULL,
	state_changed_at TEXT NOT NULL,
	created_at       TEXT NOT NULL,
	created_by       TEXT NOT NULL,
	state_changed_by TEXT NOT NULL,
	version          INTEGER NOT NULL
)
-- +statement
CREATE UNIQUE INDEX ux_attestation_keys_active ON attestation_keys (tenant_id) WHERE state = 'active'
-- +statement
CREATE INDEX ix_attestation_keys_tenant ON attestation_keys (tenant_id)
-- +statement
CREATE TABLE statement_counters (
	tenant_id TEXT PRIMARY KEY REFERENCES tenants (id) ON DELETE NO ACTION,
	seq       INTEGER NOT NULL
)
-- +statement
CREATE TABLE statements (
	tenant_id TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	seq       INTEGER NOT NULL,
	issuer_id TEXT NOT NULL,
	body_hash TEXT NOT NULL,
	withdrawn INTEGER NOT NULL,
	statement TEXT NOT NULL,
	signature TEXT NOT NULL,
	issued_at TEXT NOT NULL,
	PRIMARY KEY (tenant_id, seq)
)
-- +statement
CREATE INDEX ix_statements_body ON statements (tenant_id, body_hash, seq)
-- +statement
CREATE INDEX ix_statements_issuer ON statements (tenant_id, issuer_id, seq)
-- +statement
CREATE TABLE feeds (
	id              TEXT PRIMARY KEY,
	tenant_id       TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name            TEXT NOT NULL,
	url             TEXT NOT NULL,
	credential      TEXT,
	kinds           TEXT NOT NULL,
	readers         INTEGER NOT NULL,
	offer           TEXT NOT NULL CHECK (offer IN ('upstreams', 'channels')),
	state           TEXT NOT NULL CHECK (state IN ('active', 'paused')),
	version         INTEGER NOT NULL,
	requested_at    TEXT,
	next_run_at     TEXT,
	last_run_at     TEXT,
	last_run        TEXT,
	status          TEXT NOT NULL CHECK (status IN ('new', 'ok', 'unauthorized', 'failing')),
	last_success_at TEXT,
	last_error      TEXT,
	created_at      TEXT NOT NULL,
	created_by      TEXT NOT NULL,
	UNIQUE (tenant_id, name)
)
-- +statement
CREATE TABLE feed_keys (
	feed_id     TEXT NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	fingerprint TEXT NOT NULL,
	PRIMARY KEY (feed_id, fingerprint)
)
-- +statement
CREATE TABLE feed_channels (
	feed_id    TEXT NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	channel_id TEXT NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	PRIMARY KEY (feed_id, channel_id)
)
-- +statement
CREATE INDEX ix_feed_channels_channel ON feed_channels (channel_id)
-- +statement
CREATE TABLE feed_cursors (
	feed_id   TEXT NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	body_hash TEXT NOT NULL,
	seq       INTEGER NOT NULL,
	PRIMARY KEY (feed_id, body_hash)
)
-- +statement
ALTER TABLE attachments ADD COLUMN seq INTEGER
