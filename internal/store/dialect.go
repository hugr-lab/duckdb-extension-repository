package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Dialect holds what differs between the three engines. Queries are written with ? placeholders and
// rebound per dialect.
type Dialect struct {
	Name string // postgres | sqlserver | sqlite

	rebind     func(string) string
	unique     func(error) bool
	foreignKey func(error) bool
	retryable  func(error) bool
	// begin runs right after BEGIN with the store's lock timeout: SQL Server session settings
	// (SessionInitSQL only runs on a reset, not on a new connection, so it cannot be relied on) and
	// PostgreSQL's lock_timeout.
	begin func(ctx context.Context, tx *sql.Tx, lockTimeout time.Duration) error
	// lock takes an exclusive lock on key until the transaction ends, waiting at most lockTimeout
	// (a timeout is a retryable error). It is the first statement of a transaction that needs it.
	lock func(ctx context.Context, tx *sql.Tx, key string, lockTimeout time.Duration) error
	// timeArg converts a time for a query argument; times are always UTC, microsecond precision.
	timeArg func(time.Time) any
}

// TimeLayout is how SQLite stores timestamps: RFC 3339, six fraction digits, Z.
const TimeLayout = "2006-01-02T15:04:05.000000Z"

func numbered(prefix string) func(string) string {
	return func(q string) string {
		var b strings.Builder
		n := 0
		for i := 0; i < len(q); i++ {
			if q[i] == '?' {
				n++
				b.WriteString(prefix)
				b.WriteString(strconv.Itoa(n))
				continue
			}
			b.WriteByte(q[i])
		}
		return b.String()
	}
}

func pgCode(err error) string {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func mssqlNumber(err error) int32 {
	var e mssql.Error
	if errors.As(err, &e) {
		return e.SQLErrorNumber()
	}
	return 0
}

func sqliteCode(err error) int {
	var e *sqlite.Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return 0
}

// Postgres is the PostgreSQL dialect.
var Postgres = &Dialect{
	Name:       "postgres",
	rebind:     numbered("$"),
	unique:     func(err error) bool { return pgCode(err) == "23505" },
	foreignKey: func(err error) bool { return pgCode(err) == "23503" },
	retryable: func(err error) bool {
		c := pgCode(err)
		return c == "40001" || c == "40P01" || c == "55P03"
	},
	begin: func(ctx context.Context, tx *sql.Tx, lt time.Duration) error {
		_, err := tx.ExecContext(ctx, "SELECT set_config('lock_timeout', $1, true)", strconv.FormatInt(lt.Milliseconds(), 10))
		return err
	},
	lock: func(ctx context.Context, tx *sql.Tx, key string, _ time.Duration) error {
		// waits at most lock_timeout (set in begin); a timeout is 55P03, retryable
		_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key)
		return err
	},
	timeArg: func(t time.Time) any { return t.UTC() },
}

// SQLServer is the SQL Server dialect.
var SQLServer = &Dialect{
	Name:   "sqlserver",
	rebind: numbered("@p"),
	unique: func(err error) bool {
		n := mssqlNumber(err)
		return n == 2627 || n == 2601
	},
	// 547 is any constraint conflict (a CHECK too); a foreign key's message names it
	foreignKey: func(err error) bool { return mssqlNumber(err) == 547 && strings.Contains(err.Error(), "FOREIGN KEY") },
	retryable: func(err error) bool {
		n := mssqlNumber(err)
		return n == 1205 || n == 1222 || n == 51000
	},
	begin: func(ctx context.Context, tx *sql.Tx, lt time.Duration) error {
		_, err := tx.ExecContext(ctx, "SET XACT_ABORT ON; SET LOCK_TIMEOUT "+strconv.FormatInt(lt.Milliseconds(), 10))
		return err
	},
	lock: func(ctx context.Context, tx *sql.Tx, key string, lt time.Duration) error {
		// sp_getapplock reports failure through its return code, not an error.
		_, err := tx.ExecContext(ctx, `DECLARE @r int;
EXEC @r = sp_getapplock @Resource = @p1, @LockMode = 'Exclusive', @LockOwner = 'Transaction', @LockTimeout = @p2;
IF @r < 0 THROW 51000, 'kista: could not take the application lock', 1;`, key, lt.Milliseconds())
		return err
	},
	timeArg: func(t time.Time) any { return t.UTC() },
}

// SQLite is the SQLite dialect. Every transaction is BEGIN IMMEDIATE (_txlock=immediate), which
// already holds the database write lock, so lock is a no-op.
var SQLite = &Dialect{
	Name:   "sqlite",
	rebind: func(q string) string { return q },
	unique: func(err error) bool {
		c := sqliteCode(err)
		return c == sqlite3.SQLITE_CONSTRAINT_UNIQUE || c == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	},
	foreignKey: func(err error) bool { return sqliteCode(err) == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY },
	retryable: func(err error) bool {
		c := sqliteCode(err) & 0xff
		return c == sqlite3.SQLITE_BUSY || c == sqlite3.SQLITE_LOCKED
	},
	begin:   func(context.Context, *sql.Tx, time.Duration) error { return nil },
	lock:    func(context.Context, *sql.Tx, string, time.Duration) error { return nil },
	timeArg: func(t time.Time) any { return t.UTC().Format(TimeLayout) },
}

// scanTime scans a timestamp column of any dialect: time.Time (PostgreSQL, SQL Server) or text
// (SQLite).
type scanTime struct{ t *time.Time }

func (s scanTime) Scan(v any) error {
	switch x := v.(type) {
	case time.Time:
		*s.t = x.UTC()
	case string:
		t, err := time.Parse(TimeLayout, x)
		if err != nil {
			return err
		}
		*s.t = t
	case []byte:
		t, err := time.Parse(TimeLayout, string(x))
		if err != nil {
			return err
		}
		*s.t = t
	case nil:
		*s.t = time.Time{}
	default:
		return errors.New("store: unexpected timestamp type")
	}
	return nil
}
