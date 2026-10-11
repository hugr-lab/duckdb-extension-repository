-- spec 0017: extension groups, their members, and grants on them (apart from grants, so a binary
-- that does not know groups never reads a group grant as a tenant- or channel-wide one)
CREATE TABLE extension_groups (
	id          varchar(36) PRIMARY KEY,
	tenant_id   varchar(36) NOT NULL REFERENCES tenants (id) ON DELETE NO ACTION,
	name        varchar(63) NOT NULL,
	description varchar(256) NOT NULL,
	version     bigint NOT NULL,
	created_at  timestamptz NOT NULL,
	created_by  varchar(400) NOT NULL,
	CONSTRAINT uq_extension_groups_name UNIQUE (tenant_id, name),
	CONSTRAINT uq_extension_groups_tenant UNIQUE (id, tenant_id)
)
-- +statement
CREATE TABLE extension_group_members (
	group_id   varchar(36) NOT NULL,
	tenant_id  varchar(36) NOT NULL,
	extension  varchar(64) NOT NULL,
	added_at   timestamptz NOT NULL,
	added_by   varchar(400) NOT NULL,
	CONSTRAINT pk_extension_group_members PRIMARY KEY (group_id, extension),
	CONSTRAINT fk_extension_group_members_group FOREIGN KEY (group_id, tenant_id) REFERENCES extension_groups (id, tenant_id) ON DELETE CASCADE
)
-- +statement
CREATE INDEX ix_extension_group_members_tenant ON extension_group_members (tenant_id, extension)
-- +statement
CREATE TABLE extension_group_grants (
	id           varchar(36) PRIMARY KEY,
	tenant_id    varchar(36) NOT NULL,
	group_id     varchar(36) NOT NULL,
	issuer_id    varchar(36),
	publisher_id varchar(36),
	kind         varchar(16) NOT NULL,
	value        varchar(256) NOT NULL,
	channel_id   varchar(36),
	verbs        varchar(100) NOT NULL,
	created_at   timestamptz NOT NULL,
	created_by   varchar(400) NOT NULL,
	CONSTRAINT ck_extension_group_grants_kind CHECK (kind IN ('subject', 'role', 'group', 'client', 'issuer', 'publisher')),
	CONSTRAINT ck_extension_group_grants_principal CHECK ((issuer_id IS NULL) <> (publisher_id IS NULL) AND (kind = 'publisher') = (publisher_id IS NOT NULL)),
	CONSTRAINT fk_extension_group_grants_group FOREIGN KEY (group_id, tenant_id) REFERENCES extension_groups (id, tenant_id) ON DELETE NO ACTION,
	CONSTRAINT fk_extension_group_grants_issuer FOREIGN KEY (issuer_id, tenant_id) REFERENCES issuers (id, tenant_id) ON DELETE CASCADE,
	CONSTRAINT fk_extension_group_grants_publisher FOREIGN KEY (publisher_id, tenant_id) REFERENCES publishers (id, tenant_id) ON DELETE CASCADE,
	CONSTRAINT fk_extension_group_grants_channel FOREIGN KEY (channel_id, tenant_id) REFERENCES channels (id, tenant_id) ON DELETE NO ACTION
)
-- +statement
CREATE INDEX ix_extension_group_grants_tenant ON extension_group_grants (tenant_id)
-- +statement
CREATE INDEX ix_extension_group_grants_group ON extension_group_grants (group_id)
-- +statement
CREATE INDEX ix_extension_group_grants_issuer ON extension_group_grants (issuer_id)
-- +statement
CREATE INDEX ix_extension_group_grants_publisher ON extension_group_grants (publisher_id)
