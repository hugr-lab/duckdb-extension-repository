-- spec 0015: an issuer record's console client (a public client for the administration console)
CREATE TABLE issuer_console_clients (
	issuer_id          TEXT PRIMARY KEY REFERENCES issuers (id) ON DELETE CASCADE,
	client_id          TEXT NOT NULL,
	scopes             TEXT NOT NULL,
	audience_parameter TEXT NOT NULL,
	audience           TEXT NOT NULL,
	created_at         TEXT NOT NULL,
	created_by         TEXT NOT NULL
)
