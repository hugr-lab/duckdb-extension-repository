package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

// rotating is a login with a stale password until it is told of a rejection (spec 0027).
type rotating struct {
	mu       sync.Mutex
	good     string
	stale    bool
	rejected int
	wrong    []string // rejections of another password than the stale one
}

func (r *rotating) Password(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stale {
		return "stale-password", nil
	}
	return r.good, nil
}

func (r *rotating) Rejected(pw string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pw != "stale-password" {
		r.wrong = append(r.wrong, pw)
		return
	}
	r.stale = false
	r.rejected++
}

type fakeConnector struct{ err error }

// Connect uses the stale password, as a driver's connect would ask the login for it.
func (f fakeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	notePassword(ctx, "stale-password")
	return nil, f.err
}
func (fakeConnector) Driver() driver.Driver { return nil }

func TestRejected(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		rejected func(error) bool
		want     bool
	}{
		"pg no cause":         {errors.New("failed to connect"), pgRejected, false},
		"pg 28P01 wrapped":    {fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01"}), pgRejected, true},
		"pg 28000":            {&pgconn.PgError{Code: "28000"}, pgRejected, false},
		"pg other":            {errors.New("dial tcp: refused"), pgRejected, false},
		"mssql 18456":         {mssql.Error{Number: 18456}, msRejected, true},
		"mssql 18456 wrapped": {fmt.Errorf("x: %w", mssql.Error{Number: 18456}), msRejected, true},
		"mssql 4060":          {mssql.Error{Number: 4060}, msRejected, false},
	} {
		t.Run(name, func(t *testing.T) {
			l := &rotating{}
			c := withRejecter(fakeConnector{tc.err}, l, tc.rejected)
			if _, err := c.Connect(context.Background()); err == nil || err.Error() != tc.err.Error() {
				t.Fatalf("the error changed: %v", err)
			}
			if got := l.rejected == 1; got != tc.want {
				t.Fatalf("rejected %d", l.rejected)
			}
		})
	}
	// a login that does not want to hear keeps its connector
	if _, ok := withRejecter(fakeConnector{}, PasswordLogin{}, pgRejected).(rejecting); ok {
		t.Fatal("wrapped a login without Rejected")
	}
}

// On a real server: a stale password is rejected, the login is told, and the next attempt succeeds.
func TestRejectedOnServers(t *testing.T) {
	ctx := context.Background()
	for _, db := range []struct{ kind, dsnEnv, pwEnv string }{
		{"postgres", "KISTA_TEST_POSTGRES", "KISTA_TEST_POSTGRES_PASSWORD"},
		{"sqlserver", "KISTA_TEST_SQLSERVER", "KISTA_TEST_SQLSERVER_PASSWORD"},
	} {
		t.Run(db.kind, func(t *testing.T) {
			dsn := os.Getenv(db.dsnEnv)
			if dsn == "" {
				t.Skip(db.dsnEnv + " is not set")
			}
			l := &rotating{good: os.Getenv(db.pwEnv), stale: true}
			open := func() (*Store, error) {
				if db.kind == "postgres" {
					return OpenPostgres(ctx, dsn, l, 2, nil)
				}
				return OpenSQLServer(ctx, dsn, l, 2)
			}
			if s, err := open(); err == nil {
				s.Close()
				t.Fatal("opened with a stale password")
			}
			if l.rejected != 1 || len(l.wrong) != 0 {
				t.Fatalf("the login was told %d times of the stale password, and of %v", l.rejected, l.wrong)
			}
			s, err := open()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.Login != Login(l) {
				t.Fatal("the store does not name its login")
			}
		})
	}
}
