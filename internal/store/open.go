package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
	_ "modernc.org/sqlite" // the "sqlite" database/sql driver

	"github.com/hugr-lab/duckdb-extension-repository/internal/secretfile"
)

// Login supplies the database password (or token) for every new connection, so a DSN never holds
// one.
type Login interface {
	Password(ctx context.Context) (string, error)
}

// Rejecter is a login told when the database rejected the password it gave (PostgreSQL SQLSTATE
// 28P01, SQL Server error 18456), so a cached password is not used again (spec 0027). The attempt
// that was rejected fails; the pool's next one asks the login again.
type Rejecter interface {
	Rejected()
}

// rejecting tells a Rejecter login about a rejected password on the driver's connect path.
type rejecting struct {
	driver.Connector
	login    Rejecter
	rejected func(error) bool
}

func (r rejecting) Connect(ctx context.Context) (driver.Conn, error) {
	c, err := r.Connector.Connect(ctx)
	if err != nil && r.rejected(err) {
		r.login.Rejected()
	}
	return c, err
}

// Close closes the inner connector when it holds anything.
func (r rejecting) Close() error {
	if c, ok := r.Connector.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// withRejecter wraps a connector when the login wants to hear about rejected passwords.
func withRejecter(c driver.Connector, login Login, rejected func(error) bool) driver.Connector {
	if r, ok := login.(Rejecter); ok {
		return rejecting{Connector: c, login: r, rejected: rejected}
	}
	return c
}

// pgRejected is PostgreSQL's invalid_password.
func pgRejected(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "28P01"
}

// msRejected is SQL Server's "Login failed for user".
func msRejected(err error) bool {
	var me mssql.Error
	return errors.As(err, &me) && me.Number == 18456
}

// Target is the connection a login is for: a token login binds its token to it (spec 0020).
type Target struct {
	Host string
	Port uint16
	User string
}

// TokenLogin makes a token for each new connection from the connection's target: RDS IAM and
// Cloud SQL IAM authentication (spec 0020). It needs a single-host DSN.
type TokenLogin struct {
	For func(ctx context.Context, t Target) (string, error)
}

// Password implements Login; a token login needs the target, so the store calls PasswordFor.
func (TokenLogin) Password(context.Context) (string, error) {
	return "", errors.New("store: a token login needs the connection's host")
}

// PasswordFor returns the token for the connection.
func (l TokenLogin) PasswordFor(ctx context.Context, t Target) (string, error) { return l.For(ctx, t) }

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
		b, err := secretfile.Read(p.File, secretfile.Secret, secretfile.SmallLimit)
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

var kvRootCert = regexp.MustCompile(`(?i)(^|[\s;:&?])sslrootcert\s*=`)

// DSNHasRootCert reports whether a DSN names a CA file (sslrootcert).
func DSNHasRootCert(dsn string) bool {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		for k := range u.Query() {
			if strings.EqualFold(k, "sslrootcert") {
				return true
			}
		}
	}
	return kvRootCert.MatchString(dsn)
}

// ValidCAPEM reports whether s is one or more PEM certificates and nothing else.
func ValidCAPEM(s string) bool {
	_, err := caPool([]byte(s))
	return err == nil
}

func caPool(b []byte) (*x509.CertPool, error) {
	pool, n := x509.NewCertPool(), 0
	for rest := b; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			if strings.TrimSpace(string(rest)) != "" {
				return nil, errors.New("store: ca_pem holds something that is not a PEM certificate")
			}
			break
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if blk.Type != "CERTIFICATE" || err != nil {
			return nil, errors.New("store: ca_pem holds something that is not a PEM certificate")
		}
		pool.AddCert(c)
		n++
	}
	if n == 0 {
		return nil, errors.New("store: ca_pem holds no certificate")
	}
	return pool, nil
}

// applyCA makes caPEM the only CA every TLS host of cfg is checked against.
func applyCA(cfg *pgx.ConnConfig, caPEM []byte) error {
	pool, err := caPool(caPEM)
	if err != nil {
		return err
	}
	used := false
	for _, h := range pgHosts(cfg) {
		if h.tls != nil {
			h.tls.RootCAs, used = pool, true
		}
	}
	if !used {
		return errors.New("store: ca_pem is given, but no host is reached over TLS")
	}
	return nil
}

// PGTarget is what a PostgreSQL DSN connects to, as pgx parses it.
type PGTarget struct {
	Hosts int    // distinct host:port pairs, the main one and every fallback
	Host  string // the main host
	User  string // the user pgx would log in as
	TLS   bool   // some host is reached over TLS
}

// PostgresTarget parses a PostgreSQL DSN the way OpenPostgres does.
func PostgresTarget(dsn string) (PGTarget, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return PGTarget{}, errors.New("store: not a PostgreSQL DSN")
	}
	t := PGTarget{Hosts: pgDistinctHosts(cfg), Host: cfg.Host, User: cfg.User}
	for _, h := range pgHosts(cfg) {
		t.TLS = t.TLS || h.tls != nil
	}
	return t, nil
}

// pgDistinctHosts counts the host:port pairs pgx may dial: sslmode prefer and allow add a fallback
// for the same host (TLS, then plaintext), which is still one host.
func pgDistinctHosts(cfg *pgx.ConnConfig) int {
	seen := map[string]bool{net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))): true}
	for _, f := range cfg.Fallbacks {
		seen[net.JoinHostPort(f.Host, strconv.Itoa(int(f.Port)))] = true
	}
	return len(seen)
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

// DSNUser is the user a DSN of the given kind (postgres, sqlserver) logs in as, by the driver's own
// parsing.
func DSNUser(kind, dsn string) (string, error) {
	switch kind {
	case "postgres":
		t, err := PostgresTarget(dsn)
		return t.User, err
	case "sqlserver":
		cfg, err := msdsn.Parse(dsn)
		if err != nil {
			return "", errors.New("store: not a SQL Server DSN")
		}
		return cfg.User, nil
	}
	return "", fmt.Errorf("store: %s has no DSN user", kind)
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
	// EventClients reduces client addresses in events (spec 0010: full, truncated or none; ""
	// is truncated).
	EventClients string
	// EventSinks gives an event's pending sinks: the bits of the sinks that take its tenant (spec
	// 0010 phase 1b); nil: none.
	EventSinks func(tenantID string) int
	// Login is the login new connections use; nil for SQLite. Set by the opener.
	Login Login
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

// OpenPostgres opens PostgreSQL. Off loopback the DSN must have sslmode=verify-full. caPEM, when
// given, is the only CA the server's certificate is checked against (spec 0020: store.ca_pem).
func OpenPostgres(ctx context.Context, dsn string, login Login, maxOpen int, caPEM []byte) (*Store, error) {
	if DSNHasPassword(dsn) {
		return nil, errDSNPassword
	}
	if len(caPEM) > 0 && (DSNHasRootCert(dsn) || os.Getenv("PGSSLROOTCERT") != "") { // before parsing: pgx reads the file
		return nil, errors.New("store: ca_pem and sslrootcert (the DSN's or PGSSLROOTCERT) do not go together")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("store: not a PostgreSQL DSN")
	}
	if len(caPEM) > 0 {
		if err := applyCA(cfg, caPEM); err != nil {
			return nil, err
		}
	}
	if err := pgCheckTLS(cfg); err != nil {
		return nil, err
	}
	tl, tokens := login.(TokenLogin)
	if tokens && pgDistinctHosts(cfg) > 1 {
		return nil, errors.New("store: a token login needs a DSN with one host")
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	cfg.Password = "" // never from PGPASSWORD or .pgpass: only the login
	connector := stdlib.GetConnector(*cfg, stdlib.OptionBeforeConnect(func(ctx context.Context, cc *pgx.ConnConfig) error {
		if login == nil {
			return nil
		}
		var pw string
		var err error
		if tokens {
			pw, err = tl.PasswordFor(ctx, Target{Host: cc.Host, Port: cc.Port, User: cc.User})
		} else {
			pw, err = login.Password(ctx)
		}
		if err != nil {
			return err
		}
		cc.Password = pw
		return nil
	}))
	db := sql.OpenDB(withRejecter(connector, login, pgRejected))
	configurePool(db, maxOpen)
	if err := pingTimeout(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	s := newStore(db, Postgres)
	s.Login = login
	return s, nil
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
		connector = withRejecter(&passwordConnector{cfg: cfg, login: login}, login, msRejected)
	}
	db := sql.OpenDB(connector)
	configurePool(db, maxOpen)
	if err := pingTimeout(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	s := newStore(db, SQLServer)
	s.Login = login
	return s, nil
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
