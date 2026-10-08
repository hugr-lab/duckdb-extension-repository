package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations
var migrationFiles embed.FS

// migration is one embedded file: its version, name, the oldest binary schema level that can still
// work with the database after it, and its statements.
type migration struct {
	version    int
	name       string
	minReader  int
	statements []string
}

var (
	migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)
	minReaderLine = regexp.MustCompile(`(?m)^-- \+min_reader (\d+)\s*$`)
	statementLine = regexp.MustCompile(`(?m)^-- \+statement\s*$`)
	crlf          = strings.NewReplacer("\r\n", "\n")
)

// ErrSchemaTooNew is returned when the database needs a newer binary than this one.
var ErrSchemaTooNew = errors.New("store: the database needs a newer kista binary")

// ErrSchemaBehind is returned by Check when migrations are pending.
var ErrSchemaBehind = errors.New("store: the database has migrations to apply")

func loadMigrations(dialect string) ([]migration, error) {
	return parseMigrations(migrationFiles, path.Join("migrations", dialect))
}

// parseMigrations reads NNNN_name.sql files from dir: each file's statements are separated by
// "-- +statement" lines, and "-- +min_reader N" sets the level from that file on (a file without it
// keeps the previous level). Files must be numbered 1..n.
func parseMigrations(fsys fs.FS, dir string) ([]migration, error) {
	dialect := path.Base(dir)
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []migration
	minReader := 0
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("store: unexpected migration file %s/%s", dialect, e.Name())
		}
		raw, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		body := crlf.Replace(string(raw))
		v, _ := strconv.Atoi(m[1])
		if r := minReaderLine.FindStringSubmatch(body); r != nil {
			minReader, _ = strconv.Atoi(r[1])
		}
		var stmts []string
		for _, s := range statementLine.Split(body, -1) {
			s = strings.TrimSpace(minReaderLine.ReplaceAllString(s, ""))
			if s != "" {
				stmts = append(stmts, s)
			}
		}
		out = append(out, migration{version: v, name: m[2], minReader: minReader, statements: stmts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: %s migrations are not numbered 1..n", dialect)
		}
	}
	return out, nil
}

// SchemaLevel is the newest migration this binary knows for the store's dialect.
func (s *Store) SchemaLevel() (int, error) {
	ms, err := loadMigrations(s.d.Name)
	if err != nil {
		return 0, err
	}
	return len(ms), nil
}

func (s *Store) bootstrapSQL() string {
	switch s.d.Name {
	case "sqlserver":
		// unqualified, like every other statement: it resolves in the login's default schema
		return `IF OBJECT_ID(N'schema_migrations', N'U') IS NULL
CREATE TABLE schema_migrations (
	version    int NOT NULL PRIMARY KEY,
	name       nvarchar(200) NOT NULL,
	min_reader int NOT NULL,
	applied_at datetime2(6) NOT NULL
)`
	case "postgres":
		return `CREATE TABLE IF NOT EXISTS schema_migrations (
	version int NOT NULL PRIMARY KEY, name varchar(200) NOT NULL, min_reader int NOT NULL,
	applied_at timestamptz NOT NULL)`
	}
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER NOT NULL PRIMARY KEY, name TEXT NOT NULL, min_reader INTEGER NOT NULL,
	applied_at TEXT NOT NULL)`
}

// Migrate applies pending migrations. Every run holds the migration lock first, then creates the
// bookkeeping table if needed and re-reads what is applied, so concurrent replicas never apply a
// migration twice. It refuses a database whose min_reader exceeds this binary's schema level.
func (s *Store) Migrate(ctx context.Context) error {
	ms, err := loadMigrations(s.d.Name)
	if err != nil {
		return err
	}
	for {
		next, err := s.migrateOne(ctx, ms)
		if err != nil {
			return err
		}
		if !next {
			return nil
		}
	}
}

// migrateOne applies the first pending migration in its own transaction; it reports whether one was
// applied.
func (s *Store) migrateOne(ctx context.Context, ms []migration) (bool, error) {
	applied := false
	err := s.tx(ctx, []string{"kista/migrate"}, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, s.bootstrapSQL()); err != nil {
			return fmt.Errorf("store: creating schema_migrations: %w", err)
		}
		current, minReader, err := s.schemaState(ctx, tx.tx)
		if err != nil {
			return err
		}
		if minReader > len(ms) {
			return ErrSchemaTooNew
		}
		if current >= len(ms) {
			return nil
		}
		m := ms[current]
		for i, stmt := range m.statements {
			if _, err := tx.tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("store: migration %04d_%s, statement %d: %w", m.version, m.name, i+1, err)
			}
		}
		if _, err := tx.exec(ctx, "INSERT INTO schema_migrations (version, name, min_reader, applied_at) VALUES (?, ?, ?, ?)",
			m.version, m.name, m.minReader, s.d.timeArg(s.Now())); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func (s *Store) schemaState(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (current, minReader int, err error) {
	var cur, mr sql.NullInt64
	err = q.QueryRowContext(ctx, "SELECT MAX(version), MAX(min_reader) FROM schema_migrations").Scan(&cur, &mr)
	return int(cur.Int64), int(mr.Int64), err
}

// Check verifies, without changing anything, that this binary can work with the database:
// ErrSchemaTooNew if the database needs a newer binary, ErrSchemaBehind if migrations are pending.
func (s *Store) Check(ctx context.Context) error {
	ms, err := loadMigrations(s.d.Name)
	if err != nil {
		return err
	}
	current, minReader, err := s.schemaState(ctx, s.db)
	if err != nil {
		if missing, merr := s.migrationsTableMissing(ctx); merr == nil && missing {
			return ErrSchemaBehind
		}
		return fmt.Errorf("store: reading schema_migrations: %w", err)
	}
	if minReader > len(ms) {
		return ErrSchemaTooNew
	}
	if current < len(ms) {
		return ErrSchemaBehind
	}
	return nil
}

func (s *Store) migrationsTableMissing(ctx context.Context) (bool, error) {
	var q string
	switch s.d.Name {
	case "postgres":
		q = "SELECT COUNT(*) FROM pg_catalog.pg_tables WHERE tablename = 'schema_migrations' AND schemaname = ANY (current_schemas(false))"
	case "sqlserver":
		q = "SELECT CASE WHEN OBJECT_ID(N'schema_migrations', N'U') IS NULL THEN 0 ELSE 1 END"
	default:
		q = "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'"
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// Ready pings the database and checks the schema.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: %w", sanitize(err))
	}
	return s.Check(ctx)
}
