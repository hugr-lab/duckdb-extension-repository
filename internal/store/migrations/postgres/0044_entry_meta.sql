-- spec 0036 phase 1: a catalogue entry's metadata (tenant, type, source, name): a summary and an
-- icon, a PNG in the tenant's storage domain (icons/<sha256>), with who wrote it last and the last
-- accepted signed write's signed-at and body hash (replay protection). The icons' claims and
-- tombstones are spec 0016's, in tables of their own. Additive: an older replica ignores the tables.
CREATE TABLE entry_meta (
	tenant_id    varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type varchar(32) NOT NULL,
	source       varchar(64) NOT NULL,
	name         varchar(64) NOT NULL,
	summary      varchar(160) NULL,
	icon_sha256  varchar(64) NULL,
	icon_width   int NULL,
	icon_height  int NULL,
	icon_bytes   int NULL,
	set_by       varchar(400) NOT NULL,
	set_at       timestamptz NOT NULL,
	signed_at    timestamptz NULL,
	signed_body  varchar(64) NULL,
	PRIMARY KEY (tenant_id, package_type, source, name)
)
-- +statement
CREATE INDEX ix_entry_meta_icon ON entry_meta (icon_sha256)
-- +statement
CREATE TABLE icon_claims (
	domain     varchar(16) NOT NULL,
	sha256     varchar(64) NOT NULL,
	claim_id   varchar(36) NOT NULL,
	expires_at timestamptz NOT NULL,
	PRIMARY KEY (domain, sha256, claim_id)
)
-- +statement
CREATE TABLE icon_tombstones (
	domain      varchar(16) NOT NULL,
	sha256      varchar(64) NOT NULL,
	since       timestamptz NOT NULL,
	state       varchar(16) NOT NULL CHECK (state IN ('marked', 'deleting')),
	deleting_at timestamptz,
	PRIMARY KEY (domain, sha256)
)
