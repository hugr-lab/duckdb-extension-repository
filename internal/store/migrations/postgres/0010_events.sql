-- +min_reader 10
CREATE TABLE events (
	id         varchar(36) PRIMARY KEY,
	tenant_id  varchar(36) NOT NULL,
	at         timestamptz NOT NULL,
	kind       varchar(64) NOT NULL,
	v          integer NOT NULL,
	outcome    varchar(16) NOT NULL,
	actor      varchar(400) NOT NULL,
	actor_name varchar(256),
	subject    varchar(400) NOT NULL,
	data       text NOT NULL,
	client     varchar(64),
	request    varchar(64) NOT NULL,
	pending    integer NOT NULL
)
-- +statement
CREATE INDEX ix_events_tenant_at ON events (tenant_id, at, id)
-- +statement
CREATE INDEX ix_events_tenant_kind ON events (tenant_id, kind, at)
-- +statement
CREATE INDEX ix_events_tenant_subject ON events (tenant_id, subject, at)
-- +statement
CREATE INDEX ix_events_pending ON events (at, id) WHERE pending <> 0
-- +statement
CREATE TABLE event_sinks (
	name           varchar(64) PRIMARY KEY,
	bit            integer NOT NULL UNIQUE,
	added_at       timestamptz NOT NULL,
	last_listed_at timestamptz NOT NULL
)
