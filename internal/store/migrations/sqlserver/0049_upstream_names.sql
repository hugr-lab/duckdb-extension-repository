-- +min_reader 49
-- spec 0039 PR 2: "*" on a mirror of a kista channel. A run reads the remote's names
-- (<prefix>/-/names) and keeps the ones it takes (upstream_names): they join the matrix as listed
-- names. An older replica would run a "*" mirror with no names, so it no longer runs here.
CREATE TABLE upstream_names (
	upstream_id   nvarchar(36) COLLATE Latin1_General_100_BIN2 NOT NULL REFERENCES upstreams (id) ON DELETE NO ACTION,
	name          nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	discovered_at datetime2(6) NOT NULL,
	PRIMARY KEY (upstream_id, name)
)
