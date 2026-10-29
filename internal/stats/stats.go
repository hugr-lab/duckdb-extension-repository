// Package stats keeps download statistics (spec 0010 phase 2a): counts per (channel, release,
// DuckDB version, day) without principal or address, kept in memory and added to the store every
// minute; the first install of a (principal, release, client network, day) on a replica; and an
// hourly pass that counts finished days' distinct installers from the install events.
package stats

import (
	"cmp"
	"container/list"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Counts counts in memory per key and writes the counts every minute: downloads (spec 0010 phase
// 2a) and usage reports (spec 0012 phase 1b), each with its own budget.
type Counts[K comparable] struct {
	Write func(ctx context.Context, counts map[K]int64) (done []K, err error)
	Log   *slog.Logger
	What  string        // what is counted, for the logs: "downloads"
	Max   int           // keys waiting at most: at half they are written at once, beyond Max not counted
	Every time.Duration // default 1m

	mu      sync.Mutex
	counts  map[K]int64
	lost    int64 // units not counted since the last log
	dropped atomic.Int64
	failing bool // the last flush failed: wait for the ticker
	full    chan struct{}
	once    sync.Once
}

// Downloads counts downloads: 100,000 keys waiting are written at once, and beyond 200,000 (the
// store failing) further downloads are not counted, and logged.
func Downloads(st *store.Store, log *slog.Logger) *Counts[store.DownloadKey] {
	return &Counts[store.DownloadKey]{Write: st.AddDownloadCounts, Log: log, What: "downloads", Max: 200000}
}

// Usage counts usage reports: a budget of its own, 100,000 keys waiting, so that a burst of reports
// never takes download counts with it.
func Usage(st *store.Store, log *slog.Logger) *Counts[store.UsageKey] {
	return &Counts[store.UsageKey]{Write: st.AddUsageCounts, Log: log, What: "usage reports", Max: 100000}
}

func (c *Counts[K]) init() {
	c.once.Do(func() {
		c.counts = map[K]int64{}
		c.full = make(chan struct{}, 1)
		c.Every = cmp.Or(c.Every, time.Minute)
		c.Max = cmp.Or(c.Max, 100000)
	})
}

// Add counts one.
func (c *Counts[K]) Add(k K) {
	c.AddAll(map[K]int64{k: 1})
}

// AddAll counts all of counts, or none of them when its new keys would take the keys waiting beyond
// Max (false: dropped, and counted in Dropped).
func (c *Counts[K]) AddAll(counts map[K]int64) bool {
	c.init()
	c.mu.Lock()
	added := 0
	for k := range counts {
		if _, ok := c.counts[k]; !ok {
			added++
		}
	}
	if len(c.counts)+added > c.Max {
		c.lost++
		c.mu.Unlock()
		c.dropped.Add(1)
		return false
	}
	for k, n := range counts {
		c.counts[k] = sum(c.counts[k], n)
	}
	full := len(c.counts) >= c.Max/2 && !c.failing
	c.mu.Unlock()
	if full {
		select {
		case c.full <- struct{}{}:
		default:
		}
	}
	return true
}

// MaxCount is the most a count holds: sums stop there (a client's claims cannot overflow them).
const MaxCount = store.MaxCount

// sum adds two counts, saturating at MaxCount.
func sum(a, b int64) int64 {
	if a > MaxCount-b {
		return MaxCount
	}
	return a + b
}

// Drop counts a call dropped before it reached the counter.
func (c *Counts[K]) Drop() { c.dropped.Add(1) }

// Dropped is how many Add and AddAll calls were not counted since the start, plus the keys lost
// while the store failed.
func (c *Counts[K]) Dropped() int64 { return c.dropped.Load() }

// Run writes the counts every Every until ctx is done, then once more (for at most 10 seconds).
func (c *Counts[K]) Run(ctx context.Context) {
	c.init()
	t := time.NewTicker(c.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			c.Flush(fctx)
			return
		case <-t.C:
		case <-c.full:
		}
		c.Flush(ctx)
	}
}

// Flush writes the counts now; the counts of the batches the store refused are kept for the next
// time.
func (c *Counts[K]) Flush(ctx context.Context) {
	c.init()
	c.mu.Lock()
	counts, lost := c.counts, c.lost
	c.counts, c.lost = map[K]int64{}, 0
	c.mu.Unlock()
	if lost > 0 {
		c.Log.Error("stats: "+c.What+" not counted: too many keys waiting (the store failing, or a burst)", "not_counted", lost)
	}
	if len(counts) == 0 {
		c.mu.Lock()
		c.failing = false
		c.mu.Unlock()
		return
	}
	done, err := c.Write(ctx, counts)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failing = err != nil
	if err == nil {
		return
	}
	c.Log.Error("stats: writing the counts of "+c.What, "keys", len(counts)-len(done), "error", err)
	for _, k := range done {
		delete(counts, k)
	}
	for k, n := range counts {
		if _, ok := c.counts[k]; ok || len(c.counts) < c.Max {
			c.counts[k] = sum(c.counts[k], n)
		} else {
			c.lost += n
			c.dropped.Add(1)
		}
	}
}

// Installs remembers the installs seen on this replica: the first of a (principal, release, client
// network, day) is an event, at most MaxPerActor a day per principal and MaxActors principals a day
// (beyond, the downloads count and make no event: a token cannot flood its tenant's log). The memory is bounded (the least
// recently seen go first).
type Installs struct {
	Max         int // remembered installs; default 100,000
	MaxPerActor int // events a day per principal; default 1,000
	MaxActors   int // principals a day with events; default 100,000

	mu       sync.Mutex
	order    *list.List
	seen     map[string]*list.Element
	perActor map[[2]string]int // (principal, day) → events
	perDay   string
}

// First reports whether an install is the first of its key and within its principal's daily
// events, and remembers it. client is the client's network (a reduced address).
func (s *Installs) First(principal, releaseID, client, day string) bool {
	k := principal + "\x00" + releaseID + "\x00" + client + "\x00" + day
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.order, s.seen = list.New(), map[string]*list.Element{}
		s.Max, s.MaxPerActor, s.MaxActors = cmp.Or(s.Max, 100000), cmp.Or(s.MaxPerActor, 1000), cmp.Or(s.MaxActors, 100000)
	}
	if e, ok := s.seen[k]; ok {
		s.order.MoveToFront(e)
		return false
	}
	if day > s.perDay { // a new day; a request of the day before, late, keeps counting in the same map
		s.perActor, s.perDay = map[[2]string]int{}, day
	}
	if n, ok := s.perActor[[2]string{principal, day}]; n >= s.MaxPerActor || !ok && len(s.perActor) >= s.MaxActors {
		return false // beyond a principal's events, or beyond Max principals a day
	}
	s.perActor[[2]string{principal, day}]++
	s.seen[k] = s.order.PushFront(k)
	if s.order.Len() > s.Max {
		old := s.order.Back()
		s.order.Remove(old)
		delete(s.seen, old.Value.(string))
	}
	return true
}

// Daily counts finished days' distinct installers from the install events (on one replica at a
// time: a lease) and deletes statistics past their retention. A day is counted an hour after it
// ends (the replicas' writers have flushed its events), and only while all its events are still in
// the buffer (EventRetention); a day never counted stays without installers.
type Daily struct {
	Store          *store.Store
	Holder         string
	Retention      time.Duration // statistics.retention
	EventRetention time.Duration // events.retention (required)
	Log            *slog.Logger
	Every          time.Duration // default 1h
	// Days is how many finished days a pass looks back for one not counted yet (default 7: a pass
	// late after an outage still counts, while the buffer has the days).
	Days int
}

// grace is how long after a day's end its installers are counted.
const grace = time.Hour

// Run makes a pass every Every until ctx is done.
func (d *Daily) Run(ctx context.Context) {
	if d.Every == 0 {
		d.Every = time.Hour
	}
	t := time.NewTicker(d.Every)
	defer t.Stop()
	for {
		d.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once makes one pass if it takes the lease.
func (d *Daily) Once(ctx context.Context) {
	const lease = "kista/statistics/daily"
	if d.EventRetention <= 0 {
		d.Log.Error("stats: the daily pass needs events.retention")
		return
	}
	held, err := d.Store.AcquireLease(ctx, lease, d.Holder, 30*time.Minute)
	if err != nil || !held {
		if err != nil {
			d.Log.Error("stats: taking the daily lease", "error", err)
		}
		return
	}
	defer func() { _ = d.Store.ReleaseLease(context.WithoutCancel(ctx), lease, d.Holder) }()
	days := d.Days
	if days == 0 {
		days = 7
	}
	now := d.Store.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	for i := days; i >= 1 && ctx.Err() == nil; i-- {
		start := today.AddDate(0, 0, -i)
		if now.Before(start.AddDate(0, 0, 1).Add(grace)) || start.Before(now.Add(-d.EventRetention)) {
			continue // not finished long enough, or its first events are pruned
		}
		day := store.DayOf(start)
		done, err := d.Store.StatisticsDayDone(ctx, day)
		if err != nil {
			d.Log.Error("stats: reading a day's state", "day", day, "error", err)
			return
		}
		if done {
			continue
		}
		if err := d.count(ctx, day); err != nil {
			d.Log.Error("stats: counting a day's installers", "day", day, "error", err)
			return
		}
		if held, err := d.Store.AcquireLease(ctx, lease, d.Holder, 30*time.Minute); err != nil || !held { // a long pass keeps its lease
			return
		}
	}
	if d.Retention > 0 && ctx.Err() == nil {
		n, err := d.Store.PruneStatistics(ctx, store.DayOf(now.Add(-d.Retention)))
		if err != nil {
			d.Log.Error("stats: pruning statistics", "error", err)
		} else if n > 0 {
			d.Log.Info("stats: pruned statistics", "rows", n)
		}
	}
}

// count counts a day's distinct installers per (channel, release) and users of usage reports per
// (channel, name, version) in every tenant, and stores them.
func (d *Daily) count(ctx context.Context, day string) error {
	tenants, err := d.Store.ListTenants(ctx)
	if err != nil {
		return err
	}
	out := store.Daily{Installers: map[store.InstallerKey]int64{}, Users: map[store.UsageUserKey]int64{}}
	for _, t := range tenants {
		if !t.DeletingAt.IsZero() {
			continue // spec 0019: its statistics go with it
		}
		chans, err := d.Store.ListChannels(ctx, t.Name)
		if err != nil {
			return err
		}
		ids := map[string]string{}
		for _, c := range store.KnownChannels(chans) { // spec 0034: a type this binary does not know counts nothing
			ids[c.Name] = c.ID
		}
		// spec 0036 phase 2: an install counts under its release's source, the event's (from then
		// on), else the release's by id; "" when the release is gone
		type install struct {
			k              store.InstallerKey
			release, actor string
		}
		installers := map[store.InstallerKey]map[string]bool{}
		var older []install  // events without a source
		var unknown []string // their releases
		err = d.Store.InstallEvents(ctx, t.ID, day, func(e store.Event) error {
			var f struct {
				Release  string  `json:"release"`
				Source   *string `json:"source"`
				Name     string  `json:"name"`
				Version  string  `json:"version"`
				Platform string  `json:"platform"`
			}
			if json.Unmarshal([]byte(e.Data), &f) != nil {
				return nil
			}
			// channel:<channel>/ext:<name>/release:<id>
			ch, _, _ := strings.Cut(strings.TrimPrefix(e.Subject, "channel:"), "/")
			id := ids[ch]
			if id == "" || f.Name == "" {
				return nil
			}
			k := store.InstallerKey{TenantID: t.ID, ChannelID: id, Name: f.Name, PackageVersion: f.Version, Platform: f.Platform, Day: day}
			if f.Source != nil {
				k.Source = *f.Source
				add(installers, k, e.Actor)
				return nil
			}
			older = append(older, install{k: k, release: f.Release, actor: e.Actor})
			if f.Release != "" {
				unknown = append(unknown, f.Release)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(older) > 0 {
			srcs, err := d.Store.ReleaseSources(ctx, t.ID, unknown)
			if err != nil {
				return err
			}
			for _, in := range older {
				in.k.Source = srcs[in.release]
				add(installers, in.k, in.actor)
			}
		}
		users := map[store.UsageUserKey]map[string]bool{}
		err = d.Store.DayEvents(ctx, t.ID, "extension.use", day, func(e store.Event) error {
			var f struct {
				Version string `json:"version"`
			}
			if json.Unmarshal([]byte(e.Data), &f) != nil || f.Version == "" {
				return nil
			}
			// channel:<channel>/ext:<name>
			ch, ext, ok := strings.Cut(strings.TrimPrefix(e.Subject, "channel:"), "/ext:")
			id := ids[ch]
			if id == "" || !ok || ext == "" {
				return nil
			}
			add(users, store.UsageUserKey{TenantID: t.ID, ChannelID: id, Name: ext, PackageVersion: f.Version, Day: day}, e.Actor)
			return nil
		})
		if err != nil {
			return err
		}
		for k, who := range installers {
			out.Installers[k] = int64(len(who))
		}
		for k, who := range users {
			out.Users[k] = int64(len(who))
		}
	}
	return d.Store.PutDaily(ctx, day, out)
}

// add remembers a principal under a key.
func add[K comparable](m map[K]map[string]bool, k K, actor string) {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	m[k][actor] = true
}
