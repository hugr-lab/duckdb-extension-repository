-- +min_reader 18
-- spec 0011 phase 1b: attestation keys, the statements a tenant issues, and the feeds it subscribes
-- to; an older replica would mark documents feedable without issuing their statements
CREATE TABLE attestation_keys (
	id               varchar(36) PRIMARY KEY,
	tenant_id        varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	fingerprint      varchar(80) NOT NULL UNIQUE REFERENCES key_fingerprints (fingerprint) ON DELETE NO ACTION,
	signer_ref       varchar(2048) NOT NULL,
	public_key       bytea NOT NULL,
	state            varchar(16) NOT NULL CHECK (state IN ('trusted', 'active', 'retired')),
	trusted_since    timestamptz NOT NULL,
	state_changed_at timestamptz NOT NULL,
	created_at       timestamptz NOT NULL,
	created_by       varchar(400) NOT NULL,
	state_changed_by varchar(400) NOT NULL,
	version          bigint NOT NULL
)
-- +statement
CREATE UNIQUE INDEX ux_attestation_keys_active ON attestation_keys (tenant_id) WHERE state = 'active'
-- +statement
CREATE INDEX ix_attestation_keys_tenant ON attestation_keys (tenant_id)
-- +statement
CREATE TABLE statement_counters (
	tenant_id varchar(36) PRIMARY KEY REFERENCES tenants (id) ON DELETE NO ACTION,
	seq       bigint NOT NULL
)
-- +statement
CREATE TABLE statements (
	tenant_id varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	seq       bigint NOT NULL,
	issuer_id varchar(64) NOT NULL,
	body_hash varchar(64) NOT NULL,
	withdrawn boolean NOT NULL,
	statement text NOT NULL,
	signature varchar(512) NOT NULL,
	issued_at timestamptz NOT NULL,
	PRIMARY KEY (tenant_id, seq)
)
-- +statement
CREATE INDEX ix_statements_body ON statements (tenant_id, body_hash, seq)
-- +statement
CREATE INDEX ix_statements_issuer ON statements (tenant_id, issuer_id, seq)
-- +statement
CREATE TABLE feeds (
	id              varchar(36) PRIMARY KEY,
	tenant_id       varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name            varchar(64) NOT NULL,
	url             varchar(1000) NOT NULL,
	credential      varchar(32),
	kinds           varchar(100) NOT NULL,
	readers         boolean NOT NULL,
	offer           varchar(16) NOT NULL CHECK (offer IN ('upstreams', 'channels')),
	state           varchar(16) NOT NULL CHECK (state IN ('active', 'paused')),
	version         bigint NOT NULL,
	requested_at    timestamptz,
	next_run_at     timestamptz,
	last_run_at     timestamptz,
	last_run        text,
	status          varchar(16) NOT NULL CHECK (status IN ('new', 'ok', 'unauthorized', 'failing')),
	last_success_at timestamptz,
	last_error      varchar(1000),
	created_at      timestamptz NOT NULL,
	created_by      varchar(400) NOT NULL,
	UNIQUE (tenant_id, name)
)
-- +statement
CREATE TABLE feed_keys (
	feed_id     varchar(36) NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	fingerprint varchar(80) NOT NULL,
	PRIMARY KEY (feed_id, fingerprint)
)
-- +statement
CREATE TABLE feed_channels (
	feed_id    varchar(36) NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	channel_id varchar(36) NOT NULL REFERENCES channels (id) ON DELETE NO ACTION,
	PRIMARY KEY (feed_id, channel_id)
)
-- +statement
CREATE INDEX ix_feed_channels_channel ON feed_channels (channel_id)
-- +statement
CREATE TABLE feed_cursors (
	feed_id   varchar(36) NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
	body_hash varchar(64) NOT NULL,
	seq       bigint NOT NULL,
	PRIMARY KEY (feed_id, body_hash)
)
-- +statement
ALTER TABLE attachments ADD COLUMN seq bigint
-- +statement
-- documents marked feedable before statements existed (1a) were never offered: they start unmarked
UPDATE attachments SET feedable = false WHERE feedable = true
