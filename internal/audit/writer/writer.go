// Package writer is the asynchronous event writer (spec 0010): events outside any change
// (refusals, failures; installs in phase 2) are queued in memory per tenant and inserted in
// batches, off the request's path. A tenant over its quota drops its own events only, counted and
// written as audit.dropped; refusals are rate-limited with a count.
package writer

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Limits.
const (
	perTenant    = 1000  // events waiting per tenant
	total        = 10000 // events waiting in all
	batch        = 500
	refusalBurst = 100   // refusals a minute per tenant before they coalesce per (kind, client prefix)
	kindCeiling  = 300   // events a minute per (tenant, kind) before every further one shares one key
	limiterKeys  = 10000 // the rate limiter's keys; idle ones, then the least recently used, go
	finalFlush   = 10 * time.Second
)

// Writer queues events and inserts them in batches.
type Writer struct {
	Store *store.Store
	Log   *slog.Logger
	Every time.Duration // default 1s

	mu      sync.Mutex
	queues  map[string][]store.Event
	waiting int
	dropped map[string]map[string]int // tenant → kind → count
	limits  map[limitKey]*limit
	minute  map[string]int    // refusals this minute per tenant
	kinds   map[[2]string]int // events emitted this minute per (tenant, kind)
	trail   []trailing        // evicted keys' suppressed counts
	minAt   time.Time
	wake    chan struct{}
	once    sync.Once
}

type limitKey struct{ tenant, who, kind string }

// limit is a key's last emission and what it suppressed since, with the facts of the last
// suppressed refusal to emit their count later.
type limit struct {
	last       time.Time
	suppressed int
	req        audit.Request
	actor      string
	outcome    string
	subject    string
	fields     map[string]any
}

func (w *Writer) init() {
	w.once.Do(func() {
		w.queues, w.dropped, w.limits = map[string][]store.Event{}, map[string]map[string]int{}, map[limitKey]*limit{}
		w.minute, w.kinds = map[string]int{}, map[[2]string]int{}
		w.wake = make(chan struct{}, 1)
		if w.Every == 0 {
			w.Every = time.Second
		}
	})
}

// Add queues an event. A tenant's full queue drops its own event; when every queue together is
// full, the largest queue loses its oldest event instead, so a flooded tenant never crowds out
// another. Drops are counted for audit.dropped.
func (w *Writer) Add(e store.Event) {
	w.init()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queues[e.TenantID]) >= perTenant {
		w.drop(e.TenantID, e.Kind, 1)
		return
	}
	if w.waiting >= total {
		big := ""
		for t, q := range w.queues {
			if big == "" || len(q) > len(w.queues[big]) {
				big = t
			}
		}
		if len(w.queues[e.TenantID]) >= len(w.queues[big]) {
			w.drop(e.TenantID, e.Kind, 1)
			return
		}
		q := w.queues[big]
		w.drop(big, q[0].Kind, 1)
		w.queues[big] = q[1:]
		w.waiting--
	}
	w.queues[e.TenantID] = append(w.queues[e.TenantID], e)
	w.waiting++
	if w.waiting >= batch {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

func (w *Writer) drop(tenant, kind string, n int) {
	if w.dropped[tenant] == nil {
		w.dropped[tenant] = map[string]int{}
	}
	w.dropped[tenant][kind] += n
}

// Refusal records a refusal or a failure, rate-limited: one a second per (tenant, actor, kind),
// carrying the count of those suppressed since (a key's trailing count is emitted once it is
// quiet). Beyond refusalBurst a minute in a tenant, refusals are keyed by the client's prefix
// instead of the actor (a flood of identities is one key per network); beyond kindCeiling events a
// minute of a (tenant, kind), by nothing (one key).
func (w *Writer) Refusal(ctx context.Context, tenantID, actor string, kind audit.Kind, outcome, subject string, fields map[string]any) {
	w.init()
	if tenantID == "" {
		tenantID = audit.ServerTenant
	}
	req := audit.FromContext(ctx)
	now := time.Now()
	w.mu.Lock()
	w.newMinute(now)
	w.minute[tenantID]++
	who := actor
	switch {
	case w.kinds[[2]string{tenantID, string(kind)}] >= kindCeiling:
		who = "*"
	case w.minute[tenantID] > refusalBurst:
		who = "client:" + audit.ReduceClient(req.Client, audit.ClientTruncated)
	}
	k := limitKey{tenantID, who, string(kind)}
	l := w.limits[k]
	if l == nil {
		if len(w.limits) >= limiterKeys {
			w.evict(now)
		}
		l = &limit{}
		w.limits[k] = l
	}
	if now.Sub(l.last) < time.Second {
		l.suppressed++
		l.req, l.actor, l.outcome, l.subject, l.fields = req, actor, outcome, subject, fields
		w.mu.Unlock()
		return
	}
	count := l.suppressed + 1
	l.last, l.suppressed, l.fields = now, 0, nil
	w.kinds[[2]string{tenantID, string(kind)}]++
	w.mu.Unlock()
	w.emit(ctx, tenantID, actor, kind, outcome, subject, fields, count)
}

// Event queues an event of something that happened outside any change (an install, spec 0010
// phase 2): not rate-limited (its caller deduplicates), dropped only by a full queue.
func (w *Writer) Event(ctx context.Context, tenantID, actor string, kind audit.Kind, subject string, fields map[string]any) {
	w.init()
	e, err := w.Store.NewEvent(ctx, tenantID, actor, kind, audit.OK, subject, fields)
	if err != nil {
		w.Log.Error("audit: building an event", "kind", kind, "error", err)
		return
	}
	w.Add(e)
}

func (w *Writer) newMinute(now time.Time) {
	if now.Sub(w.minAt) >= time.Minute {
		w.minute, w.kinds, w.minAt = map[string]int{}, map[[2]string]int{}, now
	}
}

func (w *Writer) emit(ctx context.Context, tenantID, actor string, kind audit.Kind, outcome, subject string, fields map[string]any, count int) {
	f := maps.Clone(fields)
	if f == nil {
		f = map[string]any{}
	}
	f["count"] = count
	e, err := w.Store.NewEvent(ctx, tenantID, actor, kind, outcome, subject, f)
	if err != nil {
		w.Log.Error("audit: building an event", "kind", kind, "error", err)
		return
	}
	w.Add(e)
}

type trailing struct {
	k limitKey
	l limit
}

// evict makes room in the limiter (w.mu held): keys idle for a minute go, then the least recently
// used until half is free. Their suppressed counts are emitted.
func (w *Writer) evict(now time.Time) {
	var out []trailing
	for k, l := range w.limits {
		if now.Sub(l.last) > time.Minute {
			out = append(out, trailing{k, *l})
			delete(w.limits, k)
		}
	}
	if len(w.limits) >= limiterKeys/2 {
		keys := slices.Collect(maps.Keys(w.limits))
		slices.SortFunc(keys, func(a, b limitKey) int { return w.limits[a].last.Compare(w.limits[b].last) })
		for _, k := range keys[:len(keys)-limiterKeys/2+1] {
			out = append(out, trailing{k, *w.limits[k]})
			delete(w.limits, k)
		}
	}
	w.trail = append(w.trail, out...) // emitted by the next flush: emitting takes w.mu
}

// quiet takes the keys that suppressed refusals and have been quiet for a second (w.mu held).
func (w *Writer) quiet(now time.Time) []trailing {
	out := w.trail
	w.trail = nil
	for k, l := range w.limits {
		if l.suppressed > 0 && now.Sub(l.last) >= time.Second {
			out = append(out, trailing{k, *l})
			l.suppressed, l.fields = 0, nil
		}
	}
	return out
}

func (w *Writer) emitTrailing(ts []trailing) {
	for _, t := range ts {
		if t.l.suppressed == 0 {
			continue
		}
		ctx := audit.WithRequest(context.Background(), t.l.req)
		w.emit(ctx, t.k.tenant, t.l.actor, audit.Kind(t.k.kind), t.l.outcome, t.l.subject, t.l.fields, t.l.suppressed)
	}
}

// Run inserts queued events until ctx is done, then flushes (for at most 10 seconds).
func (w *Writer) Run(ctx context.Context) {
	w.init()
	t := time.NewTicker(w.Every)
	defer t.Stop()
	drops := time.NewTicker(time.Minute)
	defer drops.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlush)
			defer cancel()
			w.Close(fctx)
			return
		case <-t.C:
		case <-w.wake:
		case <-drops.C:
			w.WriteDrops(ctx)
		}
		w.Flush(ctx)
	}
}

// Close emits every trailing count, inserts what is queued and writes the drop counts.
func (w *Writer) Close(ctx context.Context) {
	w.init()
	w.mu.Lock()
	ts := w.quiet(time.Now().Add(time.Second))
	w.mu.Unlock()
	w.emitTrailing(ts)
	w.Flush(ctx)
	w.WriteDrops(ctx)
}

// Flush emits the trailing counts of quiet keys and inserts every queued event now; a batch that
// fails is counted as dropped.
func (w *Writer) Flush(ctx context.Context) {
	w.init()
	w.mu.Lock()
	ts := w.quiet(time.Now())
	w.mu.Unlock()
	w.emitTrailing(ts)
	w.mu.Lock()
	var all []store.Event
	for t, q := range w.queues {
		all = append(all, q...)
		delete(w.queues, t)
	}
	w.waiting = 0
	w.mu.Unlock()
	for len(all) > 0 {
		n := min(len(all), batch)
		if err := w.Store.InsertEvents(ctx, all[:n]); err != nil {
			w.Log.Error("audit: writing events", "count", n, "error", err)
			w.mu.Lock()
			for _, e := range all[:n] {
				w.drop(e.TenantID, e.Kind, 1)
			}
			w.mu.Unlock()
		}
		all = all[n:]
	}
}

// WriteDrops writes each tenant's drop counts as audit.dropped, straight to the store (never
// queued, so never dropped).
func (w *Writer) WriteDrops(ctx context.Context) {
	w.init()
	w.mu.Lock()
	drops := w.dropped
	w.dropped = map[string]map[string]int{}
	w.mu.Unlock()
	for tenant, counts := range drops {
		e, err := w.Store.NewEvent(ctx, tenant, "system:audit", "audit.dropped", audit.OK, "server", map[string]any{"counts": counts})
		if err == nil {
			err = w.Store.InsertEvents(ctx, []store.Event{e})
		}
		if err != nil {
			w.Log.Error("audit: recording dropped events", "tenant", tenant, "error", err)
			w.mu.Lock() // counted again next time
			for k, n := range counts {
				w.drop(tenant, k, n)
			}
			w.mu.Unlock()
		}
	}
}
