-- spec 0020 phase 4a: the bootstrap applied once; one row, its tenant by name (it outlives the
-- tenant, so a deleted tenant is not made again); pending from before its first write until it is
-- applied, so a start after a failure goes on with the tenant it made
CREATE TABLE bootstrap (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	state       TEXT NOT NULL CHECK (state IN ('pending', 'applied')),
	tenant      TEXT NOT NULL,
	digest      TEXT NOT NULL,
	seen_digest TEXT NOT NULL,
	section     TEXT NOT NULL,
	applied_at  TEXT NOT NULL,
	actor       TEXT NOT NULL
);
