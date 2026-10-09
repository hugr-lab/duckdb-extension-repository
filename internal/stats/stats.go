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
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// maxKeys bounds the counts waiting in memory: at it they are added at once; at twice it (the store
// failing) further downloads are not counted, and logged.
const maxKeys = 100000

// Counter counts downloads in memory and adds them to the store every minute.
type Counter struct {
	Store *store.Store
	Log   *slog.Logger
	Every time.Duration // default 1m

	mu      sync.Mutex
	counts  map[store.DownloadKey]int64
	lost    int64 // downloads not counted since the last log
	failing bool  // the last flush failed: wait for the ticker
	full    chan struct{}
	once    sync.Once
}

func (c *Counter) init() {
	c.once.Do(func() {
		c.counts = map[store.DownloadKey]int64{}
		c.full = make(chan struct{}, 1)
		if c.Every == 0 {
			c.Every = time.Minute
		}
	})
}

// Add counts one download.
func (c *Counter) Add(k store.DownloadKey) {
	c.init()
	c.mu.Lock()
	if _, ok := c.counts[k]; !ok && len(c.counts) >= 2*maxKeys {
		c.lost++
		c.mu.Unlock()
		return
	}
	c.counts[k]++
	full := len(c.counts) >= maxKeys && !c.failing
	c.mu.Unlock()
	if full {
		select {
		case c.full <- struct{}{}:
		default:
		}
	}
}

// Run adds the counts every Every until ctx is done, then once more (for at most 10 seconds).
func (c *Counter) Run(ctx context.Context) {
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

// Flush adds the counts now; the counts of the batches the store refused are kept for the next
// time.
func (c *Counter) Flush(ctx context.Context) {
	c.init()
	c.mu.Lock()
	counts, lost := c.counts, c.lost
	c.counts, c.lost = map[store.DownloadKey]int64{}, 0
	c.mu.Unlock()
	if lost > 0 {
		c.Log.Error("stats: downloads not counted: the store has failed for long", "downloads", lost)
	}
	done, err := c.Store.AddDownloadCounts(ctx, counts)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failing = err != nil
	if err == nil {
		return
	}
	c.Log.Error("stats: adding download counts", "keys", len(counts)-len(done), "error", err)
	for _, k := range done {
		delete(counts, k)
	}
	for k, n := range counts {
		if _, ok := c.counts[k]; ok || len(c.counts) < 2*maxKeys {
			c.counts[k] += n
		} else {
			c.lost += n
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

// count counts a day's distinct installers per (channel, release) in every tenant and stores them.
func (d *Daily) count(ctx context.Context, day string) error {
	tenants, err := d.Store.ListTenants(ctx)
	if err != nil {
		return err
	}
	rows := map[store.InstallerKey]int64{}
	for _, t := range tenants {
		chans, err := d.Store.ListChannels(ctx, t.Name)
		if err != nil {
			return err
		}
		ids := map[string]string{}
		for _, c := range chans {
			ids[c.Name] = c.ID
		}
		seen := map[store.InstallerKey]map[string]bool{}
		err = d.Store.InstallEvents(ctx, t.ID, day, func(e store.Event) error {
			var f struct {
				Name     string `json:"name"`
				Version  string `json:"version"`
				Platform string `json:"platform"`
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
			k := store.InstallerKey{TenantID: t.ID, ChannelID: id, Name: f.Name, ExtVersion: f.Version, Platform: f.Platform, Day: day}
			if seen[k] == nil {
				seen[k] = map[string]bool{}
			}
			seen[k][e.Actor] = true
			return nil
		})
		if err != nil {
			return err
		}
		for k, who := range seen {
			rows[k] = int64(len(who))
		}
	}
	return d.Store.PutInstallers(ctx, day, rows)
}
