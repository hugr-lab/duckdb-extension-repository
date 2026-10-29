-- +min_reader 47
-- spec 0038: tenant limits. Each tenant may hold its own value of four limits (NULL: the server's
-- default; -1: unlimited, the ceiling for a count; 0 or more: the limit), and a build keeps its body's
-- length, filled here from blobs and set by every insert, so a tenant's own catalogue bytes are one
-- pass over an index. A row an older replica inserts keeps NULL until the collector fills it
-- (FillBodyLen). An older replica would insert builds without body_len and check no limit, so it no
-- longer runs here.
ALTER TABLE tenants ADD COLUMN limit_bytes bigint NULL
-- +statement
ALTER TABLE tenants ADD COLUMN limit_channels integer NULL
-- +statement
ALTER TABLE tenants ADD COLUMN limit_upstreams integer NULL
-- +statement
ALTER TABLE tenants ADD COLUMN limit_publishers integer NULL
-- +statement
ALTER TABLE builds ADD COLUMN body_len bigint NULL
-- +statement
UPDATE builds b SET body_len = bl.body_len FROM tenants t, blobs bl
	WHERE t.id = b.tenant_id AND bl.domain = t.storage_domain AND bl.body_hash = b.body_hash
-- +statement
CREATE INDEX ix_builds_tenant_body ON builds (tenant_id, body_hash) INCLUDE (body_len, source, landed, withdrawn_at, id)
