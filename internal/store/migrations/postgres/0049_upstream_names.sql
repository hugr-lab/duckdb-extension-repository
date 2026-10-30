-- +min_reader 49
-- spec 0039 PR 2: "*" on a mirror of a kista channel. A run reads the remote's names
-- (<prefix>/-/names) and keeps the ones it takes (upstream_names): they join the matrix as listed
-- names. An older replica would run a "*" mirror with no names, so it no longer runs here.
CREATE TABLE upstream_names (
	upstream_id   varchar(36) NOT NULL REFERENCES upstreams (id) ON DELETE NO ACTION,
	name          varchar(64) NOT NULL,
	discovered_at timestamptz NOT NULL,
	PRIMARY KEY (upstream_id, name)
)
