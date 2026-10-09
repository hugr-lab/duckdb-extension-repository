-- +min_reader 10
CREATE TABLE events (
	id         TEXT PRIMARY KEY,
	tenant_id  TEXT NOT NULL,
	at         TEXT NOT NULL,
	kind       TEXT NOT NULL,
	v          INTEGER NOT NULL,
	outcome    TEXT NOT NULL,
	actor      TEXT NOT NULL,
	actor_name TEXT,
	subject    TEXT NOT NULL,
	data       TEXT NOT NULL,
	client     TEXT,
	request    TEXT NOT NULL,
	pending    INTEGER NOT NULL
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
	name           TEXT PRIMARY KEY,
	bit            INTEGER NOT NULL UNIQUE,
	added_at       TEXT NOT NULL,
	last_listed_at TEXT NOT NULL
)
