-- +min_reader 18
-- spec 0011 phase 1b: attestation keys, the statements a tenant issues, and the feeds it subscribes
-- to; an older replica would mark documents feedable without issuing their statements
CREATE TABLE attestation_keys (
	id               nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id        nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	fingerprint      nvarchar(80) COLLATE Latin1_General_100_BIN2 NOT NULL UNIQUE REFERENCES key_fingerprints (fingerprint) ON DELETE NO ACTION,
	signer_ref       nvarchar(2048) COLLATE Latin1_General_100_BIN2 NOT NULL,
	public_key       varbinary(1024) NOT NULL,
	state            nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('trusted', 'active', 'retired')),
	trusted_since    datetime2(6) NOT NULL,
	state_changed_at datetime2(6) NOT NULL,
	created_at       datetime2(6) NOT NULL,
	created_by       nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	state_changed_by nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	version          bigint NOT NULL
)
-- +statement
CREATE UNIQUE NONCLUSTERED INDEX ux_attestation_keys_active ON attestation_keys (tenant_id) WHERE state = N'active'
-- +statement
CREATE INDEX ix_attestation_keys_tenant ON attestation_keys (tenant_id)
-- +statement
CREATE TABLE statement_counters (
	tenant_id nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY REFERENCES tenants (id) ON DELETE NO ACTION,
	seq       bigint NOT NULL
)
-- +statement
CREATE TABLE statements (
	tenant_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	seq       bigint NOT NULL,
	issuer_id nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	body_hash nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	withdrawn bit NOT NULL,
	statement nvarchar(max) NOT NULL,
	signature nvarchar(512) COLLATE Latin1_General_100_BIN2 NOT NULL,
	issued_at datetime2(6) NOT NULL,
	PRIMARY KEY (tenant_id, seq)
)
-- +statement
CREATE INDEX ix_statements_body ON statements (tenant_id, body_hash, seq)
-- +statement
CREATE INDEX ix_statements_issuer ON statements (tenant_id, issuer_id, seq)
-- +statement
CREATE TABLE feeds (
	id              nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY,
	tenant_id       nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name            nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	url             nvarchar(1000) COLLATE Latin1_General_100_BIN2 NOT NULL,
	credential      nvarchar(32) COLLATE Latin1_General_100_BIN2 NULL,
	kinds           nvarchar(100) COLLATE Latin1_General_100_BIN2 NOT NULL,
	readers         bit NOT NULL,
	offer           nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (offer IN ('upstreams', 'channels')),
	state           nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('active', 'paused')),
	version         bigint NOT NULL,
	requested_at    datetime2(6) NULL,
	next_run_at     datetime2(6) NULL,
	last_run_at     datetime2(6) NULL,
	last_run        nvarchar(max) NULL,
	status          nvarchar(16) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (status IN ('new', 'ok', 'unauthorized', 'failing')),
	last_success_at datetime2(6) NULL,
	last_error      nvarchar(1000) COLLATE Latin1_General_100_BIN2 NULL,
	created_at      datetime2(6) NOT NULL,
	created_by      nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	UNIQUE (tenant_id, name)
)
-- +statement
CREATE TABLE feed_keys (
	feed_id     nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	fingerprint nvarchar(80) COLLATE Latin1_General_100_BIN2 NOT NULL,
	PRIMARY KEY (feed_id, fingerprint)
)
-- +statement
CREATE TABLE feed_channels (
	feed_id    nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	channel_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	PRIMARY KEY (feed_id, channel_id)
)
-- +statement
CREATE INDEX ix_feed_channels_channel ON feed_channels (channel_id)
-- +statement
CREATE TABLE feed_cursors (
	feed_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	body_hash nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	seq       bigint NOT NULL,
	PRIMARY KEY (feed_id, body_hash)
)
-- +statement
ALTER TABLE attachments ADD seq bigint NULL
-- +statement
-- documents marked feedable before statements existed (1a) were never offered: they start unmarked
UPDATE attachments SET feedable = 0 WHERE feedable = 1
