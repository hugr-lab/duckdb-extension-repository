-- spec 0015: an issuer record's console client (a public client for the administration console)
CREATE TABLE issuer_console_clients (
	issuer_id          varchar(36) PRIMARY KEY REFERENCES issuers (id) ON DELETE CASCADE,
	client_id          varchar(200) NOT NULL,
	scopes             varchar(1000) NOT NULL,
	audience_parameter varchar(64) NOT NULL,
	audience           varchar(400) NOT NULL,
	created_at         timestamptz NOT NULL,
	created_by         varchar(400) NOT NULL
)
