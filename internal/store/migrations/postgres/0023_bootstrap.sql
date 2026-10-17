-- spec 0020 phase 4a: the bootstrap applied once; one row, its tenant by name (it outlives the
-- tenant, so a deleted tenant is not made again); pending from before its first write until it is
-- applied, so a start after a failure goes on with the tenant it made
CREATE TABLE bootstrap (
	id          int PRIMARY KEY CHECK (id = 1),
	state       varchar(8) NOT NULL CHECK (state IN ('pending', 'applied')),
	tenant      varchar(63) NOT NULL,
	digest      varchar(64) NOT NULL,
	seen_digest varchar(64) NOT NULL,
	section     text NOT NULL,
	applied_at  timestamptz NOT NULL,
	actor       varchar(300) NOT NULL
);
