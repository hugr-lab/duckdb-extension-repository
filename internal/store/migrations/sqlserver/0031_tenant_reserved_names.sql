-- +min_reader 31
-- spec 0030 part 2: names reserved in a tenant for a distributor (holder: who holds it, free text).
-- In its tenant such a name is reached for publish and promote only by grants naming it, as DuckDB's
-- names are; an older replica would let a wide publish grant through, so it no longer runs here.
CREATE TABLE tenant_reserved_names (
	tenant_id nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name      nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	holder    nvarchar(128) COLLATE Latin1_General_100_BIN2 NOT NULL,
	added_at  datetime2(6) NOT NULL,
	added_by  nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	PRIMARY KEY (tenant_id, name)
)
