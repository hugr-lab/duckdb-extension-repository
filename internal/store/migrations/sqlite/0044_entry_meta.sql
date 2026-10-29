-- spec 0036 phase 1: a catalogue entry's metadata (tenant, type, source, name): a summary and an
-- icon, a PNG in the tenant's storage domain (icons/<sha256>), with who wrote it last and the last
-- accepted signed write's signed-at and body hash (replay protection). The icons' claims and
-- tombstones are spec 0016's, in tables of their own. Additive: an older replica ignores the tables.
CREATE TABLE entry_meta (
	tenant_id    TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	package_type TEXT NOT NULL,
	source       TEXT NOT NULL,
	name         TEXT NOT NULL,
	summary      TEXT NULL,
	icon_sha256  TEXT NULL,
	icon_width   INTEGER NULL,
	icon_height  INTEGER NULL,
	icon_bytes   INTEGER NULL,
	set_by       TEXT NOT NULL,
	set_at       TEXT NOT NULL,
	signed_at    TEXT NULL,
	signed_body  TEXT NULL,
	PRIMARY KEY (tenant_id, package_type, source, name)
)
-- +statement
CREATE INDEX ix_entry_meta_icon ON entry_meta (icon_sha256)
-- +statement
CREATE TABLE icon_claims (
	domain     TEXT NOT NULL,
	sha256     TEXT NOT NULL,
	claim_id   TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	PRIMARY KEY (domain, sha256, claim_id)
)
-- +statement
CREATE TABLE icon_tombstones (
	domain      TEXT NOT NULL,
	sha256      TEXT NOT NULL,
	since       TEXT NOT NULL,
	state       TEXT NOT NULL CHECK (state IN ('marked', 'deleting')),
	deleting_at TEXT,
	PRIMARY KEY (domain, sha256)
)
