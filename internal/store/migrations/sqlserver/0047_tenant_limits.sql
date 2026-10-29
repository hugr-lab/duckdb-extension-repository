-- +min_reader 47
-- spec 0038: tenant limits. Each tenant may hold its own value of four limits (NULL: the server's
-- default; -1: unlimited, the ceiling for a count; 0 or more: the limit), and a build keeps its body's
-- length, filled here from blobs and set by every insert, so a tenant's own catalogue bytes are one
-- pass over an index. A row an older replica inserts keeps NULL until the collector fills it
-- (FillBodyLen). An older replica would insert builds without body_len and check no limit, so it no
-- longer runs here.
ALTER TABLE tenants ADD limit_bytes bigint NULL
-- +statement
ALTER TABLE tenants ADD limit_channels int NULL
-- +statement
ALTER TABLE tenants ADD limit_upstreams int NULL
-- +statement
ALTER TABLE tenants ADD limit_publishers int NULL
-- +statement
ALTER TABLE builds ADD body_len bigint NULL
-- +statement
UPDATE b SET body_len = bl.body_len FROM builds b JOIN tenants t ON t.id = b.tenant_id
	JOIN blobs bl ON bl.domain = t.storage_domain AND bl.body_hash = b.body_hash
-- +statement
CREATE INDEX ix_builds_tenant_body ON builds (tenant_id, body_hash) INCLUDE (body_len, source, landed, withdrawn_at, id)
