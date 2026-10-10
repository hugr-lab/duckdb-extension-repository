-- spec 0015: an issuer record's console client (a public client for the administration console)
CREATE TABLE issuer_console_clients (
	issuer_id          nvarchar(36) COLLATE Latin1_General_100_BIN2 PRIMARY KEY REFERENCES issuers (id) ON DELETE CASCADE,
	client_id          nvarchar(200) COLLATE Latin1_General_100_BIN2 NOT NULL,
	scopes             nvarchar(1000) COLLATE Latin1_General_100_BIN2 NOT NULL,
	audience_parameter nvarchar(64) COLLATE Latin1_General_100_BIN2 NOT NULL,
	audience           nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL,
	created_at         datetime2(6) NOT NULL,
	created_by         nvarchar(400) COLLATE Latin1_General_100_BIN2 NOT NULL
)
