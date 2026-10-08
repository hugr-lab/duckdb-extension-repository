-- +min_reader 5
ALTER TABLE builds ADD COLUMN checked INTEGER NOT NULL DEFAULT 0
-- +statement
ALTER TABLE releases ADD COLUMN origin TEXT NOT NULL DEFAULT 'admin'
-- +statement
ALTER TABLE releases ADD COLUMN provenance TEXT
-- +statement
CREATE TABLE blocks (
	tenant_id  TEXT NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	body_hash  TEXT NOT NULL,
	reason     TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL,
	PRIMARY KEY (tenant_id, body_hash)
)
