package store

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
	_ "modernc.org/sqlite" // the "sqlite" database/sql driver
)

// Login supplies the database password (or token) for every new connection, so a DSN never holds
// one.
type Login interface {
	Password(ctx context.Context) (string, error)
}

// PasswordLogin reads the password from an environment variable or a file, on every connection, so
// a rotated secret is picked up without a restart.
type PasswordLogin struct {
	Env  string
	File string
}

// Password implements Login. Errors name the source, never the value.
func (p PasswordLogin) Password(context.Context) (string, error) {
	switch {
	case p.Env != "":
		v, ok := os.LookupEnv(p.Env)
		if !ok || v == "" {
			return "", fmt.Errorf("store: password variable %s is not set", p.Env)
		}
		return v, nil
	case p.File != "":
		b, err := os.ReadFile(p.File)
		if err != nil {
			return "", fmt.Errorf("store: reading the password file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return "", nil
}

// Entra scopes for token login.
const (
	ScopePostgres  = "https://ossrdbms-aad.database.windows.net/.default"
	ScopeSQLServer = "https://database.windows.net/.default"
)

// EntraLogin gets a Microsoft Entra token for the database on every connection.
type EntraLogin struct {
	Credential azcore.TokenCredential
	Scope      string
}

// Password implements Login.
func (e EntraLogin) Password(ctx context.Context) (string, error) {
	tok, err := e.Credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{e.Scope}})
	if err != nil {
		return "", errors.New("store: the service's Azure credential did not get a database token")
	}
	return tok.Token, nil
}

var (
	errDSNPassword = errors.New("store: the DSN must not contain a password; use the store login")
	kvPassword     = regexp.MustCompile(`(?i)(^|[\s;:&?])(password|pwd|sslpassword)\s*=`)
)

// DSNHasPassword reports whether a DSN carries a password: URL userinfo, a password query
// parameter, or a password= / pwd= keyword.
func DSNHasPassword(dsn string) bool {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		if _, ok := u.User.Password(); ok {
			return true
		}
		for k := range u.Query() {
			if strings.EqualFold(k, "password") || strings.EqualFold(k, "pwd") {
				return true
			}
		}
	}
	return kvPassword.MatchString(dsn)
}

// IsLoopbackHost reports whether a host name or address is the local machine.
func IsLoopbackHost(host string) bool { return isLoopback(host) }

func isLoopback(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasPrefix(host, "/") { // a Unix socket path is local
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// pgHosts returns every host pgx may connect to for a parsed config: the main one and every
// fallback (multi-host DSNs), each with its TLS settings.
func pgHosts(cfg *pgx.ConnConfig) []struct {
	host string
	tls  *tls.Config
} {
	out := []struct {
		host string
		tls  *tls.Config
	}{{cfg.Host, cfg.TLSConfig}}
	for _, f := range cfg.Fallbacks {
		out = append(out, struct {
			host string
			tls  *tls.Config
		}{f.Host, f.TLSConfig})
	}
	return out
}

// pgCheckTLS requires verified TLS (a TLS config that verifies the certificate and names the server)
// for every host pgx may use that is not loopback. It judges the parsed config, never the DSN text,
// so repeated or contradicting parameters cannot slip past it.
func pgCheckTLS(cfg *pgx.ConnConfig) error {
	for _, h := range pgHosts(cfg) {
		if isLoopback(h.host) {
			continue
		}
		if h.tls == nil || h.tls.InsecureSkipVerify || h.tls.ServerName == "" {
			return errors.New("store: PostgreSQL off loopback needs sslmode=verify-full on every host")
		}
	}
	return nil
}

// msHosts returns the SQL Server host and failover partner (if any).
func msHosts(cfg msdsn.Config) []string {
	hosts := []string{cfg.Host}
	if cfg.FailOverPartner != "" {
		p := cfg.FailOverPartner
		if h, _, err := net.SplitHostPort(p); err == nil {
			p = h
		}
		hosts = append(hosts, strings.SplitN(p, `\`, 2)[0])
	}
	return hosts
}

// DSNIsLocal reports whether every host a DSN of the given kind (postgres, sqlserver) can reach is a
// loopback address, judged from the driver's own parsing (multi-host and failover included).
func DSNIsLocal(kind, dsn string) (bool, error) {
	switch kind {
	case "postgres":
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			return false, errors.New("store: not a PostgreSQL DSN")
		}
		for _, h := range pgHosts(cfg) {
			if !isLoopback(h.host) {
				return false, nil
			}
		}
		return true, nil
	case "sqlserver":
		cfg, err := msdsn.Parse(dsn)
		if err != nil {
			return false, errors.New("store: not a SQL Server DSN")
		}
		for _, h := range msHosts(cfg) {
			if !isLoopback(h) {
				return false, nil
			}
		}
		return true, nil
	}
	return false, fmt.Errorf("store: unknown kind %q", kind)
}

// Store is the metadata store.
type Store struct {
	db *sql.DB
	d  *Dialect
	// Now is the clock; tests replace it. Times are UTC with microsecond precision.
	Now func() time.Time
	// LockTimeout bounds waiting for a lock (PostgreSQL lock_timeout, SQL Server LOCK_TIMEOUT and
	// sp_getapplock); a timeout is retried, then reported as ErrConflict.
	LockTimeout time.Duration
}

func newStore(db *sql.DB, d *Dialect) *Store {
	return &Store{db: db, d: d, LockTimeout: 10 * time.Second,
		Now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }}
}

// Dialect returns the store's dialect.
func (s *Store) Dialect() *Dialect { return s.d }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// OpenSQLite opens (creating if needed, with mode 0600) a SQLite database. The path may not contain
// '?', '#' or '%', which would change the meaning of the driver's file: URI.
func OpenSQLite(ctx context.Context, path string) (*Store, error) {
	if strings.ContainsAny(path, "?#%") {
		return nil, fmt.Errorf("%w: the SQLite path may not contain '?', '#' or '%%'", ErrInvalid)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	f.Close()
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return newStore(db, SQLite), nil
}

// OpenPostgres opens PostgreSQL. Off loopback the DSN must have sslmode=verify-full.
func OpenPostgres(ctx context.Context, dsn string, login Login, maxOpen int) (*Store, error) {
	if DSNHasPassword(dsn) {
		return nil, errDSNPassword
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("store: not a PostgreSQL DSN")
	}
	if err := pgCheckTLS(cfg); err != nil {
		return nil, err
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	cfg.Password = "" // never from PGPASSWORD or .pgpass: only the login
	db := stdlib.OpenDB(*cfg, stdlib.OptionBeforeConnect(func(ctx context.Context, cc *pgx.ConnConfig) error {
		if login == nil {
			return nil
		}
		pw, err := login.Password(ctx)
		if err != nil {
			return err
		}
		cc.Password = pw
		return nil
	}))
	configurePool(db, maxOpen)
	if err := pingTimeout(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return newStore(db, Postgres), nil
}

// OpenSQLServer opens SQL Server. Off loopback the DSN must require encryption with certificate
// verification. An EntraLogin uses the driver's access-token connector.
func OpenSQLServer(ctx context.Context, dsn string, login Login, maxOpen int) (*Store, error) {
	if DSNHasPassword(dsn) {
		return nil, errDSNPassword
	}
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		return nil, errors.New("store: not a SQL Server DSN")
	}
	if cfg.Password != "" {
		return nil, errDSNPassword
	}
	local := true
	for _, h := range msHosts(cfg) {
		local = local && isLoopback(h)
	}
	if !local { // the host or the failover partner: the same settings apply to both
		strict := cfg.Encryption == msdsn.EncryptionStrict || cfg.Encryption == msdsn.EncryptionRequired
		if !strict || cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify {
			return nil, errors.New("store: SQL Server off loopback (host or failover partner) needs encrypt=strict (or true) with certificate verification")
		}
	}
	var connector driver.Connector
	if e, ok := login.(EntraLogin); ok {
		c, err := mssql.NewConnectorWithAccessTokenProvider(dsn, e.Password)
		if err != nil {
			return nil, errors.New("store: not a SQL Server DSN")
		}
		connector = c
	} else {
		connector = &passwordConnector{cfg: cfg, login: login}
	}
	db := sql.OpenDB(connector)
	configurePool(db, maxOpen)
	if err := pingTimeout(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return newStore(db, SQLServer), nil
}

// passwordConnector sets the password from the login on every new connection.
type passwordConnector struct {
	cfg   msdsn.Config
	login Login
}

func (p *passwordConnector) Connect(ctx context.Context) (driver.Conn, error) {
	cfg := p.cfg
	if p.login != nil {
		pw, err := p.login.Password(ctx)
		if err != nil {
			return nil, err
		}
		cfg.Password = pw
	}
	return mssql.NewConnectorConfig(cfg).Connect(ctx)
}

func (p *passwordConnector) Driver() driver.Driver { return mssql.NewConnectorConfig(p.cfg).Driver() }

func configurePool(db *sql.DB, maxOpen int) {
	if maxOpen <= 0 {
		maxOpen = 10
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxLifetime(30 * time.Minute)
}

func pingTimeout(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: cannot reach the database: %w", sanitize(err))
	}
	return nil
}

// sanitize keeps driver errors from echoing connection strings.
func sanitize(err error) error {
	msg := err.Error()
	if DSNHasPassword(msg) {
		return errors.New("connection failed")
	}
	return err
}
