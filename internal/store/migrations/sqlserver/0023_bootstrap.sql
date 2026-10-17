-- spec 0020 phase 4a: the bootstrap applied once; one row, its tenant by name (it outlives the
-- tenant, so a deleted tenant is not made again); pending from before its first write until it is
-- applied, so a start after a failure goes on with the tenant it made
CREATE TABLE bootstrap (
	id          int PRIMARY KEY CHECK (id = 1),
	state       nvarchar(8) COLLATE Latin1_General_100_BIN2 NOT NULL CHECK (state IN ('pending', 'applied')),
	tenant      nvarchar(63) COLLATE Latin1_General_100_BIN2 NOT NULL,
	digest      nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	seen_digest nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	section     nvarchar(max) NOT NULL,
	applied_at  datetime2(6) NOT NULL,
	actor       nvarchar(300) NOT NULL
);
