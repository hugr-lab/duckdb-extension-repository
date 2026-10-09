// Package sinks delivers events to the configured sinks (spec 0010 phase 1b): OTLP/HTTP logs and
// JSON lines. A sink is registered by name and owns a bit in events.pending; an event's bits are
// the sinks whose tenant selection takes its tenant. One replica at a time (a lease per sink) reads
// a sink's pending events in order, checks the selection again, sends them and clears the bit:
// delivery is at least once.
package sinks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Selection is the tenants a sink takes: every tenant ("*", never the server's events), the named
// ones, and the server's events when Server.
type Selection struct {
	All    bool
	Names  map[string]bool
	Server bool
}

// SelectionOf reads a sink's configured selection.
func SelectionOf(s config.Sink) Selection {
	sel := Selection{Names: map[string]bool{}, Server: s.Server}
	for _, t := range s.Tenants {
		if t == "*" {
			sel.All = true
		} else {
			sel.Names[t] = true
		}
	}
	return sel
}

// Takes reports whether the selection takes a tenant's events; name is the tenant's name.
func (s Selection) Takes(tenantID, name string) bool {
	if tenantID == audit.ServerTenant {
		return s.Server
	}
	return s.All || s.Names[name]
}

// Resolver gives an inserted event its pending bits (store.Store.EventSinks).
type Resolver struct {
	mu    sync.Mutex           // replacing the table
	sel   map[string]Selection // by sink name
	state atomic.Pointer[table]
}

type table struct {
	masks map[string]int    // configured sink → 1 << bit
	names map[string]string // tenant id → name
}

// NewResolver returns a resolver for the configured sinks; it sets no bit until Load.
func NewResolver(cfg []config.Sink) *Resolver {
	r := &Resolver{sel: map[string]Selection{}}
	for _, s := range cfg {
		r.sel[s.Name] = SelectionOf(s)
	}
	return r
}

// Pending returns the bits of the sinks that take a tenant's events. A tenant the resolver does
// not know yet (created since its last load) gets the bits of every sink that names tenants: the
// sender checks again before it sends.
func (r *Resolver) Pending(tenantID string) int {
	t := r.state.Load()
	if t == nil {
		return 0
	}
	name, known := t.names[tenantID]
	m := 0
	for sink, mask := range t.masks {
		sel := r.sel[sink]
		if sel.Takes(tenantID, name) || !known && tenantID != audit.ServerTenant && len(sel.Names) > 0 {
			m |= mask
		}
	}
	return m
}

// Load registers the configured sinks (marking them listed now), reads the tenants' names and
// makes them the resolver's; it returns every registered sink, configured or not, tombstones
// included. With store.ErrNoSinkBit it still loads the sinks that have a bit.
func (r *Resolver) Load(ctx context.Context, st *store.Store) ([]store.Sink, error) {
	names := make([]string, 0, len(r.sel))
	for n := range r.sel {
		names = append(names, n)
	}
	reg, regErr := st.RegisterSinks(ctx, names)
	if regErr != nil && !errors.Is(regErr, store.ErrNoSinkBit) {
		return nil, regErr
	}
	masks := map[string]int{}
	for _, s := range reg {
		if _, ok := r.sel[s.Name]; ok {
			masks[s.Name] = 1 << s.Bit
		}
	}
	if err := r.loadNames(ctx, st, masks); err != nil {
		return nil, err
	}
	return reg, regErr
}

// loadNames reads the tenants' names and makes them the resolver's, with masks (the current ones
// when nil).
func (r *Resolver) loadNames(ctx context.Context, st *store.Store, masks map[string]int) error {
	ts, err := st.ListTenants(ctx)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, tn := range ts {
		names[tn.ID] = tn.Name
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if masks == nil {
		if cur := r.state.Load(); cur != nil {
			masks = cur.masks
		}
	}
	r.state.Store(&table{masks: masks, names: names})
	return nil
}

// Attach registers the configured sinks and makes the store set events' pending bits (the CLI and
// serve). A sink without a free bit is left out, reported with store.ErrNoSinkBit beside a working
// resolver.
func Attach(ctx context.Context, st *store.Store, cfg []config.Sink) (*Resolver, error) {
	r := NewResolver(cfg)
	_, err := r.Load(ctx, st)
	if err != nil && !errors.Is(err, store.ErrNoSinkBit) {
		return nil, fmt.Errorf("sinks: registering the event sinks: %w", err)
	}
	st.EventSinks = r.Pending
	return r, err
}

// Record is an event as sinks deliver it: the API's JSON with the tenant.
type Record struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id"`
	TenantName string          `json:"tenant,omitempty"`
	At         string          `json:"at"`
	Kind       string          `json:"kind"`
	V          int             `json:"v"`
	Outcome    string          `json:"outcome"`
	Actor      string          `json:"actor"`
	ActorName  string          `json:"actor_name,omitempty"`
	Subject    string          `json:"subject"`
	Data       json.RawMessage `json:"data"`
	Client     string          `json:"client,omitempty"`
	Request    string          `json:"request"`
	at         time.Time
}

// recordOf is an event as a sink sends it. A server administrator's or the CLI's identity on a
// tenant's event shows as "server", without name or address, as to the tenant's readers (spec
// 0010), unless the sink is unmasked.
func recordOf(e store.Event, tenantName string, unmasked bool) Record {
	data := json.RawMessage(e.Data)
	if !json.Valid(data) {
		data = json.RawMessage("{}")
	}
	r := Record{ID: e.ID, TenantID: e.TenantID, TenantName: tenantName, At: e.At.UTC().Format(time.RFC3339Nano), Kind: e.Kind,
		V: e.V, Outcome: e.Outcome, Actor: e.Actor, ActorName: e.ActorName, Subject: e.Subject, Data: data, Client: e.Client,
		Request: e.Request, at: e.At}
	if !unmasked && e.TenantID != audit.ServerTenant && (strings.HasPrefix(e.Actor, "server:") || strings.HasPrefix(e.Actor, "os:")) {
		r.Actor, r.ActorName, r.Client = "server", "", ""
	}
	return r
}

// Sender delivers a batch of records; an error leaves the whole batch to send again.
type Sender interface {
	Send(ctx context.Context, rs []Record) error
	Close() error
}

// Sink is a configured sink with its sender.
type Sink struct {
	Name      string
	Selection Selection
	Unmasked  bool // server identities on tenants' events as they are
	Sender    Sender
}

// Manager keeps the registry and runs a sender per sink. It runs with no sink too: it then clears
// the bits of sinks the configuration dropped.
type Manager struct {
	Store    *store.Store
	Resolver *Resolver
	Sinks    []Sink
	Holder   string
	Log      *slog.Logger
	// Intervals: Every between reads of an idle sink (1s), Refresh between registry refreshes (1m),
	// MaxBackoff the longest wait after failures (5m), Report the least time between a sink's
	// audit.sink events (10m), Stale how long a removed sink keeps its bit (10m), DropWindow the
	// span maxDrops counts in (10m).
	Every, Refresh, MaxBackoff, Report, Stale, DropWindow time.Duration
}

const (
	sendBatch  = 500
	clearBatch = 1000
	leaseTTL   = time.Minute
)

func (m *Manager) defaults() {
	for _, d := range []struct {
		p *time.Duration
		v time.Duration
	}{{&m.Every, time.Second}, {&m.Refresh, time.Minute}, {&m.MaxBackoff, 5 * time.Minute}, {&m.Report, 10 * time.Minute},
		{&m.Stale, 10 * time.Minute}, {&m.DropWindow, 10 * time.Minute}} {
		if *d.p == 0 {
			*d.p = d.v
		}
	}
}

// Run refreshes the registry and runs every sink's sender until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	m.defaults()
	var wg sync.WaitGroup
	for _, s := range m.Sinks {
		wg.Add(1)
		go func() { defer wg.Done(); m.runSink(ctx, s) }()
	}
	t := time.NewTicker(m.Refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			for _, s := range m.Sinks {
				if err := s.Sender.Close(); err != nil {
					m.Log.Error("sinks: closing", "sink", s.Name, "error", err)
				}
			}
			return
		case <-t.C:
		}
		m.refresh(ctx)
	}
}

// refresh re-registers the configured sinks (they stay listed) and reloads tenant names. A sink no
// process has listed for Stale is retired to a tombstone; a tombstone's bit is cleared from every
// event, then the tombstone goes and the bit is free.
func (m *Manager) refresh(ctx context.Context) {
	reg, err := m.Resolver.Load(ctx, m.Store)
	if err != nil {
		m.Log.Error("sinks: refreshing the registry", "error", err)
		if reg == nil {
			return
		}
	}
	cutoff := m.Store.Now().Add(-m.Stale)
	for _, s := range reg {
		if _, ours := m.Resolver.sel[s.Name]; ours || ctx.Err() != nil {
			continue
		}
		name := s.Name
		if !s.Removing {
			if s.LastListedAt.After(cutoff) {
				continue
			}
			retired, err := m.Store.RetireSink(ctx, s.Name, cutoff)
			if err != nil || !retired {
				if err != nil {
					m.Log.Error("sinks: retiring a removed sink", "sink", s.Name, "error", err)
				}
				continue
			}
			m.Log.Info("sinks: a removed sink is retired", "sink", s.Name)
			name = store.TombstoneName(s.Bit)
		}
		for ctx.Err() == nil {
			n, err := m.Store.ClearSinkBit(ctx, name, 1<<s.Bit, clearBatch)
			if err != nil {
				m.Log.Error("sinks: clearing a removed sink's events", "bit", s.Bit, "error", err)
				return
			}
			if n < clearBatch {
				break
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err := m.Store.DropSink(ctx, name); err != nil {
			m.Log.Error("sinks: freeing a removed sink's bit", "bit", s.Bit, "error", err)
		}
	}
}

// runSink delivers one sink's events while it holds the sink's lease; it waits out a failure's
// backoff holding the lease, so that another replica does not retry at once.
func (m *Manager) runSink(ctx context.Context, s Sink) {
	lease := "kista/events/sink/" + s.Name
	defer func() { _ = m.Store.ReleaseLease(context.WithoutCancel(ctx), lease, m.Holder) }()
	var (
		failing, reportedFailing bool
		class                    string
		status                   int
		backoff                  time.Duration
		reported, lastLease      time.Time
		next                     time.Time // the next attempt
		held                     bool
		// limit is the next batch's size: halved on 400 (to find a refused event) and on 413, which
		// also lowers the ceiling it grows back to
		limit, ceiling = sendBatch, sendBatch
		dropped        int       // single events dropped since the last batch the endpoint took
		firstDrop      time.Time // of those, within DropWindow
	)
	// the state the last audit.sink reported, whichever process wrote it
	if e, ok := m.lastReport(ctx, s.Name); ok {
		reportedFailing, reported = e.Outcome != audit.OK, e.At
	}
	for {
		wait := m.Every
		if !next.IsZero() {
			wait = max(0, time.Until(next))
		}
		if held {
			wait = min(wait, leaseTTL/3)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !held || time.Since(lastLease) >= leaseTTL/3 {
			var err error
			held, err = m.Store.AcquireLease(ctx, lease, m.Holder, leaseTTL)
			if err != nil {
				m.Log.Error("sinks: taking a sink's lease", "sink", s.Name, "error", err)
				held = false
			}
			lastLease = time.Now()
			if !held {
				if t := time.Now().Add(leaseTTL / 3); t.After(next) {
					next = t // a longer backoff stands
				}
				continue
			}
		}
		if time.Now().Before(next) {
			continue // backing off; the lease was renewed
		}
		if dropped > 0 && time.Since(firstDrop) >= m.DropWindow {
			dropped = 0 // at most maxDrops in DropWindow
		}
		before := dropped
		n, err := m.deliver(ctx, s, limit, &dropped)
		if ctx.Err() != nil {
			return
		}
		se, malformed := refusedAsMalformed(err)
		if malformed && limit > 1 {
			if se.Status == 413 {
				ceiling = max(1, limit/2)
			}
			limit = max(1, limit/2) // a batch the endpoint refuses as a whole: smaller ones
			next = time.Time{}
			continue
		}
		if err == nil && dropped > before {
			if before == 0 {
				firstDrop = time.Now()
			}
			// a drop is neither a failure nor a recovery; the event, not the batch size, was refused
			ceiling, next = sendBatch, time.Now()
			continue
		}
		if err != nil {
			if !failing {
				m.Log.Warn("sinks: a sink is failing", "sink", s.Name, "error", err)
			}
			failing, class, status = true, errClass(err), 0
			if se != nil {
				status = se.Status
			}
			backoff = min(max(2*backoff, m.Every), m.MaxBackoff)
			next = time.Now().Add(backoff)
		} else {
			if failing {
				m.Log.Info("sinks: a sink recovered", "sink", s.Name)
			}
			next = time.Time{}
			if n == limit {
				next = time.Now() // more are waiting
			}
			failing, backoff, limit = false, 0, min(ceiling, 2*limit)
		}
		// audit.sink when the state changes, at most once in Report
		if failing != reportedFailing && time.Since(reported) >= m.Report {
			outcome, fields := audit.OK, map[string]any{"sink": s.Name, "state": "ok"}
			if failing {
				outcome, fields["state"], fields["class"] = audit.Failed, "failing", class
				if status != 0 {
					fields["status"] = status
				}
			}
			e, err := m.Store.NewEvent(ctx, "", "system:sinks", "audit.sink", outcome, "sink:"+s.Name, fields)
			if err == nil {
				err = m.Store.InsertEvents(ctx, []store.Event{e})
			}
			if err == nil {
				reported, reportedFailing = time.Now(), failing
			}
		}
	}
}

// maxDrops is how many single events a sink drops (per replica, within DropWindow) without the
// endpoint taking a batch between them before it counts as failing: an endpoint that refuses
// everything as malformed is failing (its events wait for it), not dropping.
const maxDrops = 3

// deliver sends one batch of at most limit of a sink's pending events; it returns how many it
// read. A single event the endpoint refuses as malformed (400, 413) is dropped, counted in the
// tenant's audit.dropped (never sent to this sink: its drops never feed it), so that it never
// blocks the sink; after maxDrops with no batch taken between, the refusal is the sink's failure.
// A drop recorded whose bit then fails to clear is recorded again (at least once).
func (m *Manager) deliver(ctx context.Context, s Sink, limit int, dropped *int) (int, error) {
	mask := m.Resolver.mask(s.Name)
	if mask == 0 {
		return 0, nil
	}
	evs, err := m.Store.PendingEvents(ctx, mask, limit)
	if err != nil {
		return 0, fmt.Errorf("reading: %w", err)
	}
	if len(evs) == 0 {
		return 0, nil
	}
	names, err := m.tenantNames(ctx, evs)
	if err != nil {
		return 0, fmt.Errorf("reading tenants: %w", err)
	}
	var rs []Record
	ids := make([]string, 0, len(evs))
	for _, e := range evs {
		ids = append(ids, e.ID)
		name := names[e.TenantID]
		if !s.Selection.Takes(e.TenantID, name) {
			continue // a bit set for a tenant the sink does not take: cleared, never sent
		}
		rs = append(rs, recordOf(e, name, s.Unmasked))
	}
	if len(rs) > 0 {
		if err := s.Sender.Send(ctx, rs); err != nil {
			se, malformed := refusedAsMalformed(err)
			if len(evs) != 1 || !malformed || *dropped >= maxDrops {
				return 0, err
			}
			*dropped++
			m.Log.Warn("sinks: the endpoint refused an event; it is dropped", "sink", s.Name, "event", evs[0].ID, "status", se.Status)
			e, err := m.Store.NewEvent(ctx, evs[0].TenantID, "system:sinks", "audit.dropped", audit.OK, "server",
				map[string]any{"sink": s.Name, "counts": map[string]int{evs[0].Kind: eventCount(evs[0])}})
			if err == nil {
				e.Pending &^= mask
				err = m.Store.InsertEvents(ctx, []store.Event{e})
			}
			if err != nil {
				return 0, fmt.Errorf("recording a drop: %w", err)
			}
		} else {
			*dropped = 0
		}
	}
	if err := m.Store.ClearPending(ctx, mask, ids); err != nil {
		return 0, fmt.Errorf("clearing: %w", err)
	}
	return len(evs), nil
}

// eventCount is how many occurrences an event stands for: a rate-limited refusal's count, else 1.
func eventCount(e store.Event) int {
	var d struct {
		Count int `json:"count"`
	}
	if json.Unmarshal([]byte(e.Data), &d) == nil && d.Count > 0 {
		return d.Count
	}
	return 1
}

// lastReport reads the newest audit.sink of a sink.
func (m *Manager) lastReport(ctx context.Context, sink string) (store.Event, bool) {
	evs, err := m.Store.ListEvents(ctx, audit.ServerTenant, store.EventFilter{Kind: "audit.sink", Subject: "sink:" + sink, Limit: 100})
	if err != nil {
		m.Log.Error("sinks: reading a sink's last report", "sink", sink, "error", err)
		return store.Event{}, false
	}
	for _, e := range evs { // the subject filter is a prefix
		if e.Subject == "sink:"+sink {
			return e, true
		}
	}
	return store.Event{}, false
}

// tenantNames maps the events' tenants to their names, from the resolver's table, reloading the
// names when a tenant is new.
func (m *Manager) tenantNames(ctx context.Context, evs []store.Event) (map[string]string, error) {
	t := m.Resolver.state.Load()
	for _, e := range evs {
		if _, ok := t.names[e.TenantID]; !ok && e.TenantID != audit.ServerTenant {
			if err := m.Resolver.loadNames(ctx, m.Store, nil); err != nil {
				return nil, err
			}
			return m.Resolver.state.Load().names, nil
		}
	}
	return t.names, nil
}

func (r *Resolver) mask(sink string) int {
	t := r.state.Load()
	if t == nil {
		return 0
	}
	return t.masks[sink]
}

// refusedAsMalformed reports an endpoint's 400 or 413: the batch, not the endpoint, is the problem.
func refusedAsMalformed(err error) (*StatusError, bool) {
	var se *StatusError
	if errors.As(err, &se) && (se.Status == 400 || se.Status == 413) {
		return se, true
	}
	return se, false
}

// errClass is an error's class for audit.sink (spec 0010's fixed list), never its text, which may
// carry a host.
func errClass(err error) string {
	var se *StatusError
	switch {
	case errors.As(err, &se):
		return "egress.status"
	case errors.Is(err, egress.ErrRefused):
		return "egress.refused"
	case errors.Is(err, egress.ErrConnect), errors.Is(err, context.DeadlineExceeded):
		return "egress.connect"
	default:
		return "sink"
	}
}

// StatusError is an endpoint's answer other than success.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("sinks: the endpoint answered %d", e.Status) }
