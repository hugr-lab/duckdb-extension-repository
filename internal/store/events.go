package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
)

// Event is a recorded event (spec 0010).
type Event struct {
	ID, TenantID                   string
	At                             time.Time
	Kind                           string
	V                              int
	Outcome, Actor, ActorName      string
	Subject, Data, Client, Request string
	Pending                        int
}

// Event records an event of the change this transaction makes (spec 0010): it commits or rolls
// back with it. The request's client address (reduced), request id and the actor's display name
// come from the context. tenantID "" is the server's.
func (t *Tx) Event(ctx context.Context, tenantID, actor string, kind audit.Kind, subject string, fields map[string]any) error {
	e, err := t.s.newEvent(ctx, tenantID, actor, kind, audit.OK, subject, fields)
	if err != nil {
		return err
	}
	return t.insertEvent(ctx, e)
}

// NewEvent builds an event outside a transaction (the asynchronous writer's, spec 0010 phase 1b).
func (s *Store) NewEvent(ctx context.Context, tenantID, actor string, kind audit.Kind, outcome, subject string, fields map[string]any) (Event, error) {
	return s.newEvent(ctx, tenantID, actor, kind, outcome, subject, fields)
}

func (s *Store) newEvent(ctx context.Context, tenantID, actor string, kind audit.Kind, outcome, subject string, fields map[string]any) (Event, error) {
	data, err := audit.Data(kind, fields)
	if err != nil {
		return Event{}, err
	}
	if tenantID == "" {
		tenantID = audit.ServerTenant
	}
	r := audit.FromContext(ctx)
	mode := s.EventClients
	if mode == "" {
		mode = audit.ClientTruncated
	}
	e := Event{ID: NewID(), TenantID: tenantID, At: s.Now(), Kind: string(kind), V: audit.Version, Outcome: outcome,
		Actor: clip(actor, 400), ActorName: clip(r.ActorName, 256), Subject: clip(subject, 400), Data: data,
		Client: audit.ReduceClient(r.Client, mode), Request: r.ID}
	if e.Request == "" {
		e.Request = e.ID // background work: its own
	}
	if s.EventSinks != nil {
		e.Pending = s.EventSinks(tenantID)
	}
	return e, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

const eventCols = "id, tenant_id, at, kind, v, outcome, actor, actor_name, subject, data, client, request, pending"

func (t *Tx) insertEvent(ctx context.Context, e Event) error {
	_, err := t.exec(ctx, "INSERT INTO events ("+eventCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		e.ID, e.TenantID, t.s.d.timeArg(e.At), e.Kind, e.V, e.Outcome, e.Actor, nullable(e.ActorName), e.Subject, e.Data,
		nullable(e.Client), e.Request, e.Pending)
	return err
}

// InsertEvents writes events outside any change (the asynchronous writer's batches).
func (s *Store) InsertEvents(ctx context.Context, es []Event) error {
	return s.InTx(ctx, "", func(tx *Tx) error {
		for _, e := range es {
			if err := tx.insertEvent(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
}

func scanEvent(r interface{ Scan(...any) error }) (Event, error) {
	var e Event
	var name, client sql.NullString
	err := r.Scan(&e.ID, &e.TenantID, scanTime{&e.At}, &e.Kind, &e.V, &e.Outcome, &e.Actor, &name, &e.Subject, &e.Data,
		&client, &e.Request, &e.Pending)
	e.ActorName, e.Client = name.String, client.String
	return e, err
}

// EventFilter selects events.
type EventFilter struct {
	Since, Until         time.Time // zero: open
	Kind, Actor, Outcome string
	Subject              string // a prefix
	AfterAt              time.Time
	AfterID              string // the cursor: the last (at, id) returned
	Ascending            bool
	Limit                int
}

// ListEvents lists a tenant's events (audit.ServerTenant: the server's), newest first unless
// ascending, keyset-paged on (at, id).
func (s *Store) ListEvents(ctx context.Context, tenantID string, f EventFilter) ([]Event, error) {
	q := "SELECT " + eventCols + " FROM events WHERE tenant_id = ?"
	args := []any{tenantID}
	if !f.Since.IsZero() {
		q, args = q+" AND at >= ?", append(args, s.d.timeArg(f.Since))
	}
	if !f.Until.IsZero() {
		q, args = q+" AND at < ?", append(args, s.d.timeArg(f.Until))
	}
	for col, v := range map[string]string{"kind": f.Kind, "actor": f.Actor, "outcome": f.Outcome} {
		if v != "" {
			q, args = q+" AND "+col+" = ?", append(args, v)
		}
	}
	if f.Subject != "" {
		if s.d.Name == "sqlite" { // SQLite's LIKE ignores case: compare the prefix's characters
			q, args = q+" AND substr(subject, 1, ?) = ?", append(args, len([]rune(f.Subject)), f.Subject)
		} else { // a prefix, its LIKE metacharacters escaped (SQL Server's [ too)
			esc := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`, "[", `\[`).Replace(f.Subject)
			q, args = q+` AND subject LIKE ? ESCAPE '\'`, append(args, esc+"%")
		}
	}
	cmp, order := "<", "DESC"
	if f.Ascending {
		cmp, order = ">", "ASC"
	}
	if f.AfterID != "" {
		at := s.d.timeArg(f.AfterAt)
		q, args = q+" AND (at "+cmp+" ? OR at = ? AND id "+cmp+" ?)", append(args, at, at, f.AfterID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	// the bound is the database's: a driver reads a whole result off the wire when rows close
	q += " ORDER BY at " + order + ", id " + order
	if s.d.Name == "sqlserver" {
		q += fmt.Sprintf(" OFFSET 0 ROWS FETCH NEXT %d ROWS ONLY", limit)
	} else {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, s.d.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for len(out) < limit && rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEvent reads one event of a tenant (ErrNotFound for none).
func (s *Store) GetEvent(ctx context.Context, tenantID, id string) (Event, error) {
	e, err := scanEvent(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+eventCols+" FROM events WHERE tenant_id = ? AND id = ?"), tenantID, id))
	return e, notFound(err, "event")
}

// PruneEvents deletes up to batch events written before cutoff that every sink has (spec 0010's
// buffer), and up to batch undelivered ones written before overdue; it returns how many it deleted.
func (s *Store) PruneEvents(ctx context.Context, cutoff, overdue time.Time, batch int) (int, error) {
	n := 0
	for _, c := range []struct {
		where string
		at    time.Time
	}{{"pending = 0 AND at < ?", cutoff}, {"pending <> 0 AND at < ?", overdue}} {
		m, err := s.deleteEvents(ctx, c.where, batch, s.d.timeArg(c.at))
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}

// PruneTenantOverflow deletes the tenants' oldest events beyond max rows each, in batches.
func (s *Store) PruneTenantOverflow(ctx context.Context, max, batch int) (int, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT tenant_id, COUNT(*) FROM events GROUP BY tenant_id HAVING COUNT(*) > ?"), max)
	if err != nil {
		return 0, err
	}
	over := map[string]int{}
	for rows.Next() {
		var t string
		var c int
		if err := rows.Scan(&t, &c); err != nil {
			rows.Close()
			return 0, err
		}
		over[t] = c - max
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	total := 0
	for t, extra := range over {
		for extra > 0 && ctx.Err() == nil {
			m, err := s.deleteOldestOf(ctx, t, min(extra, batch))
			if err != nil {
				return total, err
			}
			total += m
			extra -= batch
			if m == 0 {
				break
			}
		}
	}
	return total, nil
}

// deleteEvents deletes at most n events matching where (the oldest first).
func (s *Store) deleteEvents(ctx context.Context, where string, n int, args ...any) (int, error) {
	var q string
	if s.d.Name == "sqlserver" {
		q = fmt.Sprintf("DELETE FROM events WHERE id IN (SELECT TOP (%d) id FROM events WHERE %s ORDER BY at, id)", n, where)
	} else {
		q = fmt.Sprintf("DELETE FROM events WHERE id IN (SELECT id FROM events WHERE %s ORDER BY at, id LIMIT %d)", where, n)
	}
	res, err := s.db.ExecContext(ctx, s.d.rebind(q), args...)
	if err != nil {
		return 0, err
	}
	m, err := res.RowsAffected()
	return int(m), err
}

func (s *Store) deleteOldestOf(ctx context.Context, tenantID string, n int) (int, error) {
	var q string
	if s.d.Name == "sqlserver" {
		q = fmt.Sprintf("DELETE FROM events WHERE id IN (SELECT TOP (%d) id FROM events WHERE tenant_id = ? ORDER BY at, id)", n)
	} else {
		q = fmt.Sprintf("DELETE FROM events WHERE id IN (SELECT id FROM events WHERE tenant_id = ? ORDER BY at, id LIMIT %d)", n)
	}
	res, err := s.db.ExecContext(ctx, s.d.rebind(q), tenantID)
	if err != nil {
		return 0, err
	}
	m, err := res.RowsAffected()
	return int(m), err
}
