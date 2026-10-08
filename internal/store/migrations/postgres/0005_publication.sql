-- +min_reader 5
ALTER TABLE builds ADD COLUMN checked boolean NOT NULL DEFAULT false
-- +statement
ALTER TABLE releases ADD COLUMN origin varchar(16) NOT NULL DEFAULT 'admin'
-- +statement
ALTER TABLE releases ADD COLUMN provenance varchar(4000)
-- +statement
CREATE TABLE blocks (
	tenant_id  varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	body_hash  varchar(64) NOT NULL,
	reason     varchar(400) NOT NULL,
	created_at timestamptz NOT NULL,
	created_by varchar(400) NOT NULL,
	PRIMARY KEY (tenant_id, body_hash)
)
