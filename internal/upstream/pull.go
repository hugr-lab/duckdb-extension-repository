package upstream

import (
	"context"
	"crypto/rsa"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Pull-through limits (spec 0009 phase 2).
const (
	pullQueue       = 256   // misses waiting for a worker; a full queue drops a miss
	pullQueuedShare = 32    // misses one tenant may have waiting
	pullPerTenant   = 4     // fetches at once per tenant
	pullPerMinute   = 60    // fetches a tenant may start in a minute
	maxNegative     = 10000 // cells remembered as not to fetch again yet
	keysTTL         = 5 * time.Minute
	failedTTL       = time.Minute // a failed fetch (network, store) is retried sooner than a refusal
	keysFailedTTL   = time.Minute
	cellLeaseTTL    = 2 * time.Minute
)

// Puller fetches the cells authorized callers missed (spec 0009 phase 2). Offer never blocks and
// never touches the database; workers check the upstream as it is now, take the cell's lease, run
// the intake and record the outcome. Misses and refusals are remembered for NegativeTTL.
type Puller struct {
	Service     *Service
	Holder      string        // this replica
	NegativeTTL time.Duration // default 1h
	Log         *slog.Logger

	once     sync.Once
	queue    chan release.Miss
	mu       sync.Mutex
	inflight map[string]bool // cells queued or fetching on this replica
	queued   map[string]int  // misses waiting per tenant
	tenants  map[string]int  // fetches per tenant
	started  map[string][]time.Time
	negative map[string]time.Time // cell -> until
	keys     map[string]cachedKeys
}

type cachedKeys struct {
	keys []*rsa.PublicKey
	err  error
	at   time.Time
}

func (p *Puller) init() {
	p.once.Do(func() {
		p.queue = make(chan release.Miss, pullQueue)
		p.inflight, p.queued, p.tenants, p.started = map[string]bool{}, map[string]int{}, map[string]int{}, map[string][]time.Time{}
		p.negative, p.keys = map[string]time.Time{}, map[string]cachedKeys{}
		if p.NegativeTTL == 0 {
			p.NegativeTTL = time.Hour
		}
	})
}

func cellKey(m release.Miss) string {
	return m.UpstreamID + "\x00" + m.DuckDBVersion + "\x00" + m.Platform + "\x00" + m.Name
}

// Offer queues a miss unless it is not to be fetched again yet, already queued or fetching here, its
// tenant has its share of the queue waiting, or the queue is full.
func (p *Puller) Offer(m release.Miss) {
	p.init()
	k := cellKey(m)
	p.mu.Lock()
	if until, ok := p.negative[k]; ok && time.Now().Before(until) || p.inflight[k] || p.queued[m.TenantID] >= pullQueuedShare {
		p.mu.Unlock()
		return
	}
	p.inflight[k] = true
	p.queued[m.TenantID]++
	p.mu.Unlock()
	select {
	case p.queue <- m:
	default:
		p.mu.Lock()
		delete(p.inflight, k)
		p.dequeued(m.TenantID)
		p.mu.Unlock()
	}
}

// dequeued counts a tenant's miss out of the queue (p.mu held).
func (p *Puller) dequeued(tenant string) {
	if p.queued[tenant]--; p.queued[tenant] <= 0 {
		delete(p.queued, tenant)
	}
}

// admit decides whether a tenant may start a fetch now: at most pullPerTenant at once and
// pullPerMinute a minute (p.mu held).
func (p *Puller) admit(tenant string, now time.Time) bool {
	if p.tenants[tenant] >= pullPerTenant {
		return false
	}
	recent := p.started[tenant][:0]
	for _, t := range p.started[tenant] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= pullPerMinute {
		p.started[tenant] = recent
		return false
	}
	p.started[tenant] = append(recent, now)
	p.tenants[tenant]++
	return true
}

// remember keeps a cell from being fetched again until a time; a full cache evicts the expired
// entries, then arbitrary ones (never everything at once).
func (p *Puller) remember(k string, until time.Time) {
	if len(p.negative) >= maxNegative {
		now := time.Now()
		for x, u := range p.negative {
			if !now.Before(u) {
				delete(p.negative, x)
			}
		}
		for x := range p.negative {
			if len(p.negative) < maxNegative*9/10 {
				break
			}
			delete(p.negative, x)
		}
	}
	p.negative[k] = until
}

// Run works the queue with upstreams.concurrency workers until ctx is done.
func (p *Puller) Run(ctx context.Context) {
	p.init()
	n := p.Service.Config.Concurrency
	if n <= 0 {
		n = 4
	}
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case m := <-p.queue:
					p.work(ctx, m)
				}
			}
		}()
	}
	wg.Wait()
}

// Drain works the queued misses now (tests).
func (p *Puller) Drain(ctx context.Context) {
	p.init()
	for {
		select {
		case m := <-p.queue:
			p.work(ctx, m)
		default:
			return
		}
	}
}

func (p *Puller) work(ctx context.Context, m release.Miss) {
	k := cellKey(m)
	defer func() {
		p.mu.Lock()
		delete(p.inflight, k)
		p.mu.Unlock()
	}()
	p.mu.Lock()
	p.dequeued(m.TenantID)
	if !p.admit(m.TenantID, time.Now()) {
		p.mu.Unlock()
		return // dropped: a later request retries
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.tenants[m.TenantID]--; p.tenants[m.TenantID] <= 0 {
			delete(p.tenants, m.TenantID)
		}
		p.mu.Unlock()
	}()
	outcome, err := p.fetch(ctx, m)
	if err != nil {
		p.Log.Warn("upstream: a pull-through fetch failed", "upstream", m.UpstreamID, "name", m.Name, "error", err)
	}
	// a released cell is served from now on; any other outcome is not fetched again for a while (a
	// yanked or unchanged release still misses: fetching again would change nothing)
	switch {
	case errors.Is(err, credential.ErrClient): // the operator's configuration: not asked again for a while
		p.mu.Lock()
		p.remember(k, time.Now().Add(failedTTL))
		p.mu.Unlock()
		return
	}
	switch outcome {
	case store.CellReleased, "":
	case store.CellFailed:
		p.mu.Lock()
		p.remember(k, time.Now().Add(failedTTL))
		p.mu.Unlock()
	default:
		p.mu.Lock()
		p.remember(k, time.Now().Add(p.NegativeTTL))
		p.mu.Unlock()
	}
}

// fetch runs the intake for one missed cell under its lease and returns its outcome ("" when it was
// not fetched: another replica holds the cell, or the upstream no longer takes it).
func (p *Puller) fetch(ctx context.Context, m release.Miss) (string, error) {
	s := p.Service
	var u store.Upstream
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		u, err = tx.GetUpstreamByID(ctx, m.UpstreamID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	e, ok := entry(u, m.Name)
	if !ok {
		if _, any := entry(u, "*"); any && reserved.Kind(m.Name) == "" && reserved.Canonical(m.Name) == "" {
			// "*" takes no name another upstream of the channel lists (it may have been added since
			// the snapshot)
			var owner string
			if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
				var err error
				owner, err = tx.ChannelEntryOwner(ctx, u.ChannelID, m.Name, u.ID)
				return err
			}); err != nil {
				return "", err
			}
			if owner == "" {
				e, ok = store.UpstreamEntry{Name: m.Name}, true
			}
		}
	}
	if u.Mode != store.ModePullThrough || u.State != store.UpstreamActive || !ok || !slices.Contains(u.Platforms, m.Platform) ||
		u.TenantID != m.TenantID {
		return "", nil
	}
	sc, err := s.Store.ServeChannelByID(ctx, u.ChannelID)
	if err != nil {
		return "", err
	}
	versions, err := s.Store.ChannelVersions(ctx, u.ChannelID)
	if err != nil {
		return "", err
	}
	if !slices.Contains(versions, m.DuckDBVersion) {
		return "", nil
	}
	// one fetch of a cell at a time across replicas: a lease renewed while it runs, deleted after
	lease := "kista/upstream/" + u.ID + "/" + m.DuckDBVersion + "/" + m.Platform + "/" + m.Name
	held, err := s.Store.AcquireLease(ctx, lease, p.Holder, cellLeaseTTL)
	if err != nil || !held {
		return "", err
	}
	defer func() { _ = s.Store.DeleteLease(context.WithoutCancel(ctx), lease, p.Holder) }()
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(cellLeaseTTL / 4)
		defer t.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-t.C:
				if ok, err := s.Store.AcquireLease(lctx, lease, p.Holder, cellLeaseTTL); err != nil || !ok {
					cancel()
					return
				}
			}
		}
	}()
	ctx = lctx
	keys, err := p.upstreamKeys(ctx, u)
	if err != nil {
		return store.CellFailed, err
	}
	var prev store.UpstreamCell
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		prev, err = tx.GetCell(ctx, u.ID, m.DuckDBVersion, m.Platform, m.Name)
		if errors.Is(err, store.ErrNotFound) {
			err = nil
		}
		return err
	}); err != nil {
		return "", err
	}
	base := s.prefix(u)
	pu, _ := url.Parse(base)
	perHost := s.Config.PerHost
	if perHost <= 0 {
		perHost = 2
	}
	actor := authz.Actor{Kind: authz.ActorSystem, ID: "upstream:" + sc.Tenant.Name + "/" + u.Name}
	rec := s.cell(ctx, actor, sc, u, e, keys, base, s.gates().host(pu.Host, perHost),
		cell{m.DuckDBVersion, m.Platform, m.Name}, prev, false)
	if ctx.Err() != nil {
		return "", nil // stopping, or the lease was lost: nothing is recorded
	}
	if rec.Outcome == "" { // the identity provider refuses kista's own client: logged, nothing recorded
		return "", credential.ErrClient
	}
	if rec.FetchedAt.Equal(prev.FetchedAt) && prev.Outcome != "" {
		return store.CellUnchanged, nil // a 304: the cell keeps its record
	}
	// a "*" upstream records released cells only: names probed by callers do not pile up
	_, listed := entry(u, m.Name)
	if listed || rec.Outcome == store.CellReleased {
		wctx := context.WithoutCancel(ctx)
		if err := s.Store.InTx(wctx, "", func(tx *store.Tx) error {
			if err := tx.PutCell(wctx, rec); err != nil {
				return err
			}
			// a cell turning into a refusal is an event (spec 0010), as in a run
			if refusal[rec.Outcome] && rec.Outcome != prev.Outcome {
				return tx.Event(wctx, u.TenantID, actor.String(), "upstream.rejected", "upstream:"+u.Name,
					map[string]any{"upstream": u.Name, "duckdb_version": m.DuckDBVersion, "platform": m.Platform, "name": m.Name,
						"outcome": rec.Outcome})
			}
			return nil
		}); err != nil {
			return rec.Outcome, err
		}
	}
	return rec.Outcome, nil
}

// upstreamKeys caches an upstream's verification keys for a few minutes (a repository's
// .well-known file is read at most that often).
func (p *Puller) upstreamKeys(ctx context.Context, u store.Upstream) ([]*rsa.PublicKey, error) {
	p.mu.Lock()
	c, ok := p.keys[u.ID]
	p.mu.Unlock()
	switch {
	case ok && c.err != nil && time.Since(c.at) < keysFailedTTL:
		return nil, c.err // a failing .well-known is not read on every miss
	case ok && c.err == nil && time.Since(c.at) < keysTTL:
		return filterKeys(c.keys, u), nil
	}
	keys, err := p.Service.keys(ctx, u)
	if err != nil {
		p.mu.Lock()
		p.keys[u.ID] = cachedKeys{err: err, at: time.Now()}
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Lock()
	p.keys[u.ID] = cachedKeys{keys: keys, at: time.Now()}
	p.mu.Unlock()
	return filterKeys(keys, u), nil
}

// filterKeys keeps a repository's keys that are pinned now.
func filterKeys(keys []*rsa.PublicKey, u store.Upstream) []*rsa.PublicKey {
	if u.Kind != store.UpstreamRepo {
		return keys
	}
	var out []*rsa.PublicKey
	for _, k := range keys {
		if slices.Contains(u.Keys, extfile.Fingerprint(k)) {
			out = append(out, k)
		}
	}
	return out
}

// Queued is how many misses wait on this replica (metrics).
func (p *Puller) Queued() int64 {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, q := range p.queued {
		n += q
	}
	return int64(n)
}
