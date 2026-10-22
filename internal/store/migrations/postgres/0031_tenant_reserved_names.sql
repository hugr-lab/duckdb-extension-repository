-- +min_reader 31
-- spec 0030 part 2: names reserved in a tenant for a distributor (holder: who holds it, free text).
-- In its tenant such a name is reached for publish and promote only by grants naming it, as DuckDB's
-- names are; an older replica would let a wide publish grant through, so it no longer runs here.
CREATE TABLE tenant_reserved_names (
	tenant_id varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name      varchar(64) NOT NULL,
	holder    varchar(128) NOT NULL,
	added_at  timestamptz NOT NULL,
	added_by  varchar(400) NOT NULL,
	PRIMARY KEY (tenant_id, name)
)
