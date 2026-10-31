-- +min_reader 50
-- spec 0040: the newest event time retention has removed, per tenant, so a reader following the
-- events tells events it missed from a quiet or young tenant. An older replica's pruner would
-- delete without recording it, so it no longer runs here.
CREATE TABLE event_pruned (
	tenant_id      varchar(36) PRIMARY KEY,
	pruned_through timestamptz NOT NULL
)
