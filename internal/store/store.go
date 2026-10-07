// Package store keeps kista's metadata on PostgreSQL, SQL Server or SQLite (spec 0003).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Errors. Callers test with errors.Is.
var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
	ErrConflict = errors.New("store: changed concurrently")
	ErrInvalid  = errors.New("store: invalid")
)

const maxAttempts = 8

// Tx is a transaction. Its methods are the store's writes; services compose them.
type Tx struct {
	s  *Store
	tx *sql.Tx
}

func (t *Tx) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.s.d.rebind(q), args...)
}

func (t *Tx) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.s.d.rebind(q), args...)
}

func (t *Tx) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.s.d.rebind(q), args...)
}

// Now is the store's clock.
func (t *Tx) Now() time.Time { return t.s.Now() }

// InTx runs fn in a transaction that first takes the exclusive lock lockKey (if not empty). A
// retryable engine error (deadlock, serialization, lock timeout, SQLITE_BUSY) retries the whole
// function up to 8 times with jitter, so fn must read what it needs inside the transaction. Any
// other error, ErrConflict included, rolls back and returns: a compare-and-set against a stale
// version never succeeds by repeating it.
func (s *Store) InTx(ctx context.Context, lockKey string, fn func(*Tx) error) error {
	return s.tx(ctx, lockKey, fn)
}

func (s *Store) tx(ctx context.Context, lockKey string, fn func(*Tx) error) error {
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			d := time.Duration(1<<min(attempt, 6))*5*time.Millisecond + time.Duration(rand.IntN(20))*time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		err = s.once(ctx, lockKey, fn)
		if err == nil {
			return nil
		}
		if !s.d.retryable(err) {
			return err
		}
	}
	if s.d.retryable(err) {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

func (s *Store) once(ctx context.Context, lockKey string, fn func(*Tx) error) (err error) {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		// a panic must not leave the transaction (and its locks) open on a pooled connection
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = sqlTx.Rollback()
		}
	}()
	if err = s.d.begin(ctx, sqlTx, s.LockTimeout); err != nil {
		return err
	}
	if lockKey != "" {
		if err = s.d.lock(ctx, sqlTx, lockKey, s.LockTimeout); err != nil {
			return err
		}
	}
	if err = fn(&Tx{s: s, tx: sqlTx}); err != nil {
		return err
	}
	return sqlTx.Commit()
}

// mapErr turns engine constraint errors into store errors.
func (s *Store) mapErr(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case s.d.unique(err):
		return fmt.Errorf("%w: %s", ErrExists, what)
	case s.d.foreignKey(err):
		return fmt.Errorf("%w: %s refers to something that does not exist", ErrInvalid, what)
	}
	return err
}

// NewID returns a UUIDv7 in canonical lowercase form.
func NewID() string { return uuid.Must(uuid.NewV7()).String() }

var nameRule = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var reservedNames = map[string]bool{
	"api": true, "admin": true, "healthz": true, "readyz": true, "metrics": true, "static": true, "ui": true,
}

// ValidName checks a tenant or channel name: a URL path segment [a-z0-9][a-z0-9-]{0,62}, not reserved.
func ValidName(name string) error {
	if !nameRule.MatchString(name) {
		return fmt.Errorf("%w: name %q must match [a-z0-9][a-z0-9-]{0,62}", ErrInvalid, name)
	}
	if reservedNames[name] {
		return fmt.Errorf("%w: name %q is reserved", ErrInvalid, name)
	}
	return nil
}

var versionRule = regexp.MustCompile(`^(v\d+\.\d+\.\d+(-[a-z0-9.]+)?|[0-9a-f]{10})$`)

// ValidDuckDBVersion checks a DuckDB version name: a release tag (v2.0.0) or a 10-character source id.
func ValidDuckDBVersion(name string) error {
	if !versionRule.MatchString(name) {
		return fmt.Errorf("%w: DuckDB version %q is neither a release tag nor a 10-character source id", ErrInvalid, name)
	}
	return nil
}

// ExecRaw runs one statement outside a transaction. It exists for test fixtures (creating and
// emptying databases) and is never used by kista's own code paths.
func ExecRaw(ctx context.Context, s *Store, q string, args ...any) error {
	_, err := s.db.ExecContext(ctx, s.d.rebind(q), args...)
	return err
}

// QueryRowRaw runs one query outside a transaction and scans one row; for test fixtures only.
func QueryRowRaw(ctx context.Context, s *Store, q string, dest ...any) error {
	return s.db.QueryRowContext(ctx, s.d.rebind(q)).Scan(dest...)
}

// Backup writes a consistent copy of a SQLite store to a new file at path (VACUUM INTO), created
// with mode 0600 first; it never overwrites. Other engines use their own backups.
func (s *Store) Backup(ctx context.Context, path string) error {
	if s.d.Name != "sqlite" {
		return fmt.Errorf("%w: backup is for SQLite; use the database's own backups", ErrInvalid)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	f.Close()
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
