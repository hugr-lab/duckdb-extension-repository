package store

import (
	"context"
	"database/sql"
	"errors"
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
// buffer), and up to batch undelivered ones written before overdue; it returns how many of each it
// deleted.
func (s *Store) PruneEvents(ctx context.Context, cutoff, overdue time.Time, batch int) (delivered, undelivered int, err error) {
	if delivered, err = s.deleteEvents(ctx, "pending = 0 AND at < ?", batch, s.d.timeArg(cutoff)); err != nil {
		return delivered, 0, err
	}
	undelivered, err = s.deleteEvents(ctx, "pending <> 0 AND at < ?", batch, s.d.timeArg(overdue))
	return delivered, undelivered, err
}

// PruneTenantOverflow deletes the tenants' oldest events beyond max rows each, in batches: those
// every sink has first, then undelivered ones, which it counts.
func (s *Store) PruneTenantOverflow(ctx context.Context, max, batch int) (deleted, undelivered int, err error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT tenant_id, COUNT(*) FROM events GROUP BY tenant_id HAVING COUNT(*) > ?"), max)
	if err != nil {
		return 0, 0, err
	}
	over := map[string]int{}
	for rows.Next() {
		var t string
		var c int
		if err := rows.Scan(&t, &c); err != nil {
			rows.Close()
			return 0, 0, err
		}
		over[t] = c - max
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for t, extra := range over {
		for _, delivered := range []bool{true, false} {
			for extra > 0 && ctx.Err() == nil {
				m, err := s.deleteOldestOf(ctx, t, delivered, min(extra, batch))
				if err != nil {
					return deleted, undelivered, err
				}
				deleted += m
				if !delivered {
					undelivered += m
				}
				extra -= m
				if m == 0 {
					break
				}
			}
		}
	}
	return deleted, undelivered, nil
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

func (s *Store) deleteOldestOf(ctx context.Context, tenantID string, delivered bool, n int) (int, error) {
	cond := "pending <> 0"
	if delivered {
		cond = "pending = 0"
	}
	var q string
	if s.d.Name == "sqlserver" {
		q = fmt.Sprintf("DELETE FROM events WHERE id IN (SELECT TOP (%d) id FROM events WHERE tenant_id = ? AND %s ORDER BY at, id)", n, cond)
	} else {
		q = fmt.Sprintf("DELETE FROM events WHERE id IN (SELECT id FROM events WHERE tenant_id = ? AND %s ORDER BY at, id LIMIT %d)", cond, n)
	}
	res, err := s.db.ExecContext(ctx, s.d.rebind(q), tenantID)
	if err != nil {
		return 0, err
	}
	m, err := res.RowsAffected()
	return int(m), err
}

// PendingEvents reads up to limit events still to deliver to a sink (mask: 1 << its bit), in (at,
// id) order.
func (s *Store) PendingEvents(ctx context.Context, mask, limit int) ([]Event, error) {
	q := "SELECT " + eventCols + " FROM events WHERE pending <> 0 AND (pending & ?) <> 0 ORDER BY at, id"
	if s.d.Name == "sqlserver" {
		q += fmt.Sprintf(" OFFSET 0 ROWS FETCH NEXT %d ROWS ONLY", limit)
	} else {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, s.d.rebind(q), mask)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClearPending marks events delivered to a sink (mask: 1 << its bit).
func (s *Store) ClearPending(ctx context.Context, mask int, ids []string) error {
	for len(ids) > 0 {
		n := min(len(ids), 200)
		ph := strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
		args := []any{^mask} // the complement here: a parameter's type is not inferred under ~ on PostgreSQL
		for _, id := range ids[:n] {
			args = append(args, id)
		}
		if _, err := s.db.ExecContext(ctx, s.d.rebind("UPDATE events SET pending = pending & ? WHERE id IN ("+ph+")"), args...); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

// ClearSinkBit clears a retired sink's bit (mask: 1 << bit) from up to batch events while its
// tombstone exists (another replica may have dropped it and the bit be another sink's since); it
// reports how many.
func (s *Store) ClearSinkBit(ctx context.Context, tombstoneName string, mask, batch int) (int, error) {
	const live = " AND EXISTS (SELECT 1 FROM event_sinks WHERE name = ?)"
	var q string
	if s.d.Name == "sqlserver" {
		q = fmt.Sprintf("UPDATE events SET pending = pending & ? WHERE id IN (SELECT TOP (%d) id FROM events WHERE pending <> 0 AND (pending & ?) <> 0)"+live, batch)
	} else {
		q = fmt.Sprintf("UPDATE events SET pending = pending & ? WHERE id IN (SELECT id FROM events WHERE pending <> 0 AND (pending & ?) <> 0 LIMIT %d)"+live, batch)
	}
	res, err := s.db.ExecContext(ctx, s.d.rebind(q), ^mask, mask, tombstoneName)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// Sink is a registered event sink: its name and its bit (spec 0010). A removed sink's row is
// renamed to a tombstone (Removing) that keeps its bit reserved until no event carries it.
type Sink struct {
	Name                  string
	Bit                   int
	AddedAt, LastListedAt time.Time
	Removing              bool
}

// MaxSinks is the most sinks: a bit each in events.pending.
const MaxSinks = 16

// ErrNoSinkBit is returned with the sinks registered when some configured ones found no free bit.
var ErrNoSinkBit = errors.New("store: no free event sink bit (at most 16 sinks, removed ones included until they are cleared)")

const tombstone = "~removing:"

// RegisterSinks registers the configured sinks by name (a new one gets the lowest free bit) and
// marks them listed now; it returns every registered sink, tombstones included. Names that find no
// free bit are left out and reported with ErrNoSinkBit.
func (s *Store) RegisterSinks(ctx context.Context, names []string) ([]Sink, error) {
	var out []Sink
	full := false
	err := s.InTx(ctx, "kista/events/sinks", func(tx *Tx) error {
		out, full = nil, false
		rows, err := tx.query(ctx, "SELECT name, bit, added_at, last_listed_at FROM event_sinks ORDER BY bit")
		if err != nil {
			return err
		}
		have := map[string]bool{}
		used := map[int]bool{}
		for rows.Next() {
			var k Sink
			if err := rows.Scan(&k.Name, &k.Bit, scanTime{&k.AddedAt}, scanTime{&k.LastListedAt}); err != nil {
				rows.Close()
				return err
			}
			k.Removing = strings.HasPrefix(k.Name, tombstone)
			have[k.Name], used[k.Bit] = true, true
			out = append(out, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		now := tx.Now()
		for _, n := range names {
			if have[n] {
				if _, err := tx.exec(ctx, "UPDATE event_sinks SET last_listed_at = ? WHERE name = ?", tx.s.d.timeArg(now), n); err != nil {
					return err
				}
				for i := range out {
					if out[i].Name == n {
						out[i].LastListedAt = now
					}
				}
				continue
			}
			bit := -1
			for b := range MaxSinks {
				if !used[b] {
					bit = b
					break
				}
			}
			if bit < 0 {
				full = true
				continue
			}
			used[bit] = true
			if _, err := tx.exec(ctx, "INSERT INTO event_sinks (name, bit, added_at, last_listed_at) VALUES (?, ?, ?, ?)",
				n, bit, tx.s.d.timeArg(now), tx.s.d.timeArg(now)); err != nil {
				return err
			}
			out = append(out, Sink{Name: n, Bit: bit, AddedAt: now, LastListedAt: now})
		}
		return nil
	})
	if err == nil && full {
		err = ErrNoSinkBit
	}
	return out, err
}

// RetireSink turns a sink no process has listed since before into a tombstone, under the
// registry's lock: its name is free again (a sink re-added under it gets a new bit) while its bit
// stays reserved until ClearSinkBit has cleared it and DropSink removes it. It reports whether the
// sink was retired (false: listed again since).
func (s *Store) RetireSink(ctx context.Context, name string, before time.Time) (bool, error) {
	retired := false
	err := s.InTx(ctx, "kista/events/sinks", func(tx *Tx) error {
		var bit int
		err := tx.queryRow(ctx, "SELECT bit FROM event_sinks WHERE name = ? AND last_listed_at < ?", name, tx.s.d.timeArg(before)).Scan(&bit)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		res, err := tx.exec(ctx, "UPDATE event_sinks SET name = ? WHERE name = ?", TombstoneName(bit), name)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		retired = n == 1
		return err
	})
	return retired, err
}

// TombstoneName is a retired sink's tombstone's name.
func TombstoneName(bit int) string { return fmt.Sprintf("%s%d", tombstone, bit) }

// DropSink removes a cleared tombstone: its bit is free.
func (s *Store) DropSink(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, tombstone) {
		return fmt.Errorf("%w: %s is not a removed sink", ErrInvalid, name)
	}
	_, err := s.db.ExecContext(ctx, s.d.rebind("DELETE FROM event_sinks WHERE name = ?"), name)
	return err
}

// LastEvent reads a tenant's newest event of a kind (ErrNotFound for none).
func (s *Store) LastEvent(ctx context.Context, tenantID, kind string) (Event, error) {
	evs, err := s.ListEvents(ctx, tenantID, EventFilter{Kind: kind, Limit: 1})
	if err != nil {
		return Event{}, err
	}
	if len(evs) == 0 {
		return Event{}, fmt.Errorf("%w: event", ErrNotFound)
	}
	return evs[0], nil
}

// pendingCap bounds the undelivered events PendingCounts reads.
const pendingCap = 1000000

// PendingCounts counts the events still to deliver to each sink (masks: 1 << its bit, by name), in
// one pass over at most a million undelivered events.
func (s *Store) PendingCounts(ctx context.Context, masks map[string]int) (map[string]int64, error) {
	out := map[string]int64{}
	if len(masks) == 0 {
		return out, nil
	}
	names := make([]string, 0, len(masks))
	for n := range masks {
		names = append(names, n)
	}
	sums := make([]string, len(names))
	args := make([]any, len(names))
	for i, n := range names {
		sums[i], args[i] = "SUM(CASE WHEN (pending & ?) <> 0 THEN 1 ELSE 0 END)", masks[n]
	}
	vals := make([]sql.NullInt64, len(names))
	dst := make([]any, len(names))
	for i := range vals {
		dst[i] = &vals[i]
	}
	// at most pendingCap undelivered events are read: a long backlog reads as the cap, never as a scan
	// too slow for the metrics' export
	sub := fmt.Sprintf("SELECT pending FROM events WHERE pending <> 0 LIMIT %d", pendingCap)
	if s.d.Name == "sqlserver" {
		sub = fmt.Sprintf("SELECT TOP (%d) pending FROM events WHERE pending <> 0", pendingCap)
	}
	q := "SELECT " + strings.Join(sums, ", ") + " FROM (" + sub + ") p"
	if err := s.db.QueryRowContext(ctx, s.d.rebind(q), args...).Scan(dst...); err != nil {
		return nil, err
	}
	for i, n := range names {
		out[n] = vals[i].Int64
	}
	return out, nil
}
