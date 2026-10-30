package upstream

import (
	"context"
	"crypto/rsa"
	"errors"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Pull-through limits (spec 0009 phase 2).
const (
	pullQueue       = 256   // misses waiting for a worker; a full queue drops a miss
	pullQueuedShare = 32    // misses one tenant may have waiting
	pullCallerShare = 8     // misses one caller may have waiting (spec 0032)
	pullPerTenant   = 4     // fetches at once per tenant
	pullPerMinute   = 60    // fetches a tenant may start in a minute
	maxNegative     = 10000 // cells remembered as not to fetch again yet
	keysTTL         = 5 * time.Minute
	failedTTL       = time.Minute // a failed fetch (network, store) is retried sooner than a refusal
	keysFailedTTL   = time.Minute
	cellLeaseTTL    = 2 * time.Minute
)

// Puller fetches the cells authorized callers missed (spec 0009 phase 2) and the cells asked of lazy
// mirrors (spec 0032). Offer never blocks and never touches the database; workers check the
// upstream as it is now, take the cell's lease, run the intake and record the outcome. Misses and
// refusals are remembered for NegativeTTL. Ask records a lazy mirror's ask and tells the request's
// answer.
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
	callers  map[string]int  // misses waiting per caller
	tenants  map[string]int  // fetches per tenant
	started  map[string][]time.Time
	negative map[string]time.Time // cell -> until
	keys     map[string]cachedKeys
	asks     map[string]int64 // lazy asks by outcome, cumulative (metrics)
}

// Lazy asks' outcomes (spec 0032 metrics): recorded or renewed, answered 503 (the build is coming),
// fetched (released), dropped (the queue, a share or the tenant's limits).
const (
	AskRecorded = "recorded"
	Ask503      = "answered_503"
	AskFetched  = "fetched"
	AskDropped  = "dropped"
)

type cachedKeys struct {
	keys []*rsa.PublicKey
	err  error
	at   time.Time
}

func (p *Puller) init() {
	p.once.Do(func() {
		p.queue = make(chan release.Miss, pullQueue)
		p.inflight, p.queued, p.tenants, p.started = map[string]bool{}, map[string]int{}, map[string]int{}, map[string][]time.Time{}
		p.negative, p.keys, p.callers, p.asks = map[string]time.Time{}, map[string]cachedKeys{}, map[string]int{}, map[string]int64{}
		if p.NegativeTTL == 0 {
			p.NegativeTTL = time.Hour
		}
	})
}

func cellKey(m release.Miss) string {
	return m.UpstreamID + "\x00" + m.DuckDBVersion + "\x00" + m.Platform + "\x00" + m.Name
}

// Offer queues a miss unless it is not to be fetched again yet, already queued or fetching here, its
// tenant or caller has its share of the queue waiting, or the queue is full.
func (p *Puller) Offer(m release.Miss) {
	p.init()
	k := cellKey(m)
	p.mu.Lock()
	if until, ok := p.negative[k]; ok && time.Now().Before(until) || p.inflight[k] {
		p.mu.Unlock()
		return
	}
	if p.queued[m.TenantID] >= pullQueuedShare || p.callers[m.Principal] >= pullCallerShare {
		p.dropped(m)
		p.mu.Unlock()
		return
	}
	p.inflight[k] = true
	p.queued[m.TenantID]++
	p.callers[m.Principal]++
	p.mu.Unlock()
	select {
	case p.queue <- m:
	default:
		p.mu.Lock()
		delete(p.inflight, k)
		p.dequeued(m)
		p.dropped(m)
		p.mu.Unlock()
	}
}

// dropped counts a lazy ask's offer not fetched (p.mu held): its ask stays for the next run.
func (p *Puller) dropped(m release.Miss) {
	if m.Lazy {
		p.asks[AskDropped]++
	}
}

// dequeued counts a miss out of the queue (p.mu held).
func (p *Puller) dequeued(m release.Miss) {
	if p.queued[m.TenantID]--; p.queued[m.TenantID] <= 0 {
		delete(p.queued, m.TenantID)
	}
	if p.callers[m.Principal]--; p.callers[m.Principal] <= 0 {
		delete(p.callers, m.Principal)
	}
}

// askWindow is how long after its ask a cell never fetched is answered as coming (spec 0032), so a
// fetch that never comes (an offer dropped, a replica stopped) is not answered 503 for ever; a first
// request on the plain path after it opens the window again, and is offered again. A fetch that
// cannot start (the upstream's keys unreadable, kista's client refused) records the cell failed.
const askWindow = 2 * time.Minute

// Ask records a lazy mirror's ask for a missed cell (spec 0032) and tells whether its build is
// coming: a fetch holds the cell's lease, or the cell was never fetched and was asked for within
// askWindow (a request on the plain path, which is offered, opens it). Every replica answers alike,
// from the database; the cell is read outside any write transaction, and the ask written only when
// it is new, due a renewal, or reopens the window.
func (p *Puller) Ask(ctx context.Context, plain bool, m release.Miss) (bool, error) {
	p.init()
	st, err := p.Service.Store.ReadAsk(ctx, m.UpstreamID, m.DuckDBVersion, m.Platform, m.Name,
		CellLease(m.UpstreamID, m.DuckDBVersion, m.Platform, m.Name))
	if err != nil {
		return false, err
	}
	now := p.Service.Store.Now()
	refresh := store.AskRefresh
	if plain && !st.Recorded && !st.Leased {
		refresh = askWindow
	}
	if st.AskedAt.IsZero() || now.Sub(st.AskedAt) >= refresh {
		wrote := false
		if err := p.Service.Store.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			wrote, err = tx.Ask(ctx, m.UpstreamID, m.DuckDBVersion, m.Platform, m.Name, refresh)
			return err
		}); err != nil {
			return false, err
		}
		if wrote {
			p.mu.Lock()
			p.asks[AskRecorded]++
			p.mu.Unlock()
		}
		st.AskedAt = now // ours, or another request's just now
	}
	return st.Leased || plain && !st.Recorded && now.Sub(st.AskedAt) < askWindow, nil
}

// Answered503 counts a request answered 503 because its build is coming (metrics).
func (p *Puller) Answered503() {
	p.init()
	p.mu.Lock()
	p.asks[Ask503]++
	p.mu.Unlock()
}

// Asks are the lazy asks' outcomes so far on this replica (metrics, cumulative).
func (p *Puller) Asks(context.Context) (map[string]int64, error) {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.asks), nil
}

// admit decides whether a tenant may start a fetch now: at most pullPerTenant at once, and never
// every worker (spec 0032: a tenant's fetches waiting on a host do not hold the others back), and
// pullPerMinute a minute (p.mu held).
func (p *Puller) admit(tenant string, now time.Time) bool {
	workers := p.Service.Config.Concurrency
	if workers <= 0 {
		workers = 4
	}
	if p.tenants[tenant] >= min(pullPerTenant, max(1, workers-1)) {
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
	p.dequeued(m)
	if !p.admit(m.TenantID, time.Now()) {
		p.dropped(m)
		p.mu.Unlock()
		return // dropped: a later request retries (a lazy ask: the next run fetches it)
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
	if err != nil && !isRoomRefusal(err) {
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
	case isRoomRefusal(err): // spec 0038: failed, but refused again until room is made
		p.mu.Lock()
		p.remember(k, time.Now().Add(p.NegativeTTL))
		p.mu.Unlock()
		return
	}
	switch outcome {
	case store.CellReleased:
		if m.Lazy {
			p.mu.Lock()
			p.asks[AskFetched]++
			p.mu.Unlock()
		}
	case "":
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
	e, ok := u.Entry(m.Name)
	if !ok {
		if _, any := u.Entry("*"); any && release.AnyTakes(pkgtype.DuckDBExtension, u.Kind, m.Name) { // an upstream's names are DuckDB's
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
	lazy := u.Mode == store.ModeMirror && u.FetchMode == store.FetchLazy
	if u.Mode != store.ModePullThrough && !lazy || u.State != store.UpstreamActive || !ok || !slices.Contains(u.Platforms, m.Platform) ||
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
	// one fetch of a cell at a time across replicas
	lctx, done, held, err := s.holdCell(ctx, p.Holder, u.ID, cell{m.DuckDBVersion, m.Platform, m.Name})
	if err != nil || !held {
		return "", err
	}
	defer done()
	ctx = lctx
	keys, err := p.upstreamKeys(ctx, u)
	if err != nil {
		if lazy { // spec 0032: a first request is answered 404 now, not 503 while nothing comes
			p.putFailed(ctx, u, m, "upstream: the repository's keys cannot be read")
		}
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
	gate := s.gates().host(pu.Host, perHost)
	if lazy { // spec 0032: first requests have a gate of their own, one fetch a host and tenant, so a run does not hold them back
		gate = s.gates().host("ask "+u.TenantID+" "+pu.Host, 1)
	}
	room, err := s.newBodyRoom(ctx, u) // spec 0038: checked before the download
	if err != nil {
		if lazy { // a fetch that cannot start, as above
			p.putFailed(ctx, u, m, "the catalogue's use could not be read")
		}
		return store.CellFailed, err
	}
	rec := s.cell(ctx, actor, sc, u, e, keys, base, gate, cell{m.DuckDBVersion, m.Platform, m.Name}, prev, false, triggerAsk, room)
	if ctx.Err() != nil {
		return "", nil // stopping, or the lease was lost: nothing is recorded
	}
	if rec.Outcome == atLimit { // spec 0038: no room for a cell holding no body: not requested
		if lazy && prev.Outcome == "" { // a first request is answered 404 now, not 503 while nothing comes (spec 0032)
			p.putFailed(ctx, u, m, room.noRoom().Error())
		}
		return store.CellFailed, room.noRoom()
	}
	if rec.Outcome == "" { // the identity provider refuses kista's own client: logged, nothing recorded
		if lazy { // but a lazy mirror's cell is failed (spec 0032), as above
			p.putFailed(ctx, u, m, "upstream.auth: kista's client is refused")
		}
		return "", credential.ErrClient
	}
	if rec.FetchedAt.Equal(prev.FetchedAt) && prev.Outcome != "" {
		return store.CellUnchanged, nil // a 304: the cell keeps its record
	}
	// a "*" upstream records released cells only: names probed by callers do not pile up
	_, listed := u.Entry(m.Name)
	if listed || rec.Outcome == store.CellReleased {
		wctx := context.WithoutCancel(ctx)
		if err := s.Store.InTenantTx(wctx, u.TenantID, nil, func(tx *store.Tx) error {
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
	// spec 0038: this cell refused by a limit (the room is the fetch's own): asked again within
	// NegativeTTL it would be refused again; any other failure is retried sooner
	if refused := room.refusal(); refused != nil && rec.Outcome == store.CellFailed {
		return rec.Outcome, refused
	}
	return rec.Outcome, nil
}

// putFailed records a lazy mirror's cell failed when its fetch cannot start (spec 0032): the cell
// was asked for and answered 503; the next run, or an ask after a minute, tries again.
func (p *Puller) putFailed(ctx context.Context, u store.Upstream, m release.Miss, detail string) {
	wctx := context.WithoutCancel(ctx)
	if err := p.Service.Store.InTenantTx(wctx, u.TenantID, nil, func(tx *store.Tx) error {
		return tx.PutCell(wctx, store.UpstreamCell{UpstreamID: u.ID, DuckDBVersion: m.DuckDBVersion, Platform: m.Platform, Name: m.Name,
			Outcome: store.CellFailed, Detail: detail, FetchedAt: time.Now().UTC()})
	}); err != nil {
		p.Log.Warn("upstream: recording a failed cell", "upstream", u.ID, "name", m.Name, "error", err)
	}
}

// CellLease is the name of a cell's lease (spec 0009 phase 2, 0032).
func CellLease(upstreamID, duckdbVersion, platform, name string) string {
	return "kista/upstream/" + upstreamID + "/" + duckdbVersion + "/" + platform + "/" + name
}

// holdCell takes a cell's lease, so one fetch of a cell runs at a time across replicas (a
// pull-through or lazy fetch, and a lazy upstream's run), and renews it while the fetch runs: the
// context it answers ends when the lease is lost. done ends the context and deletes the lease.
func (s *Service) holdCell(ctx context.Context, holder, upstreamID string, c cell) (context.Context, func(), bool, error) {
	lease := CellLease(upstreamID, c.version, c.platform, c.name)
	held, err := s.Store.AcquireLease(ctx, lease, holder, cellLeaseTTL)
	if err != nil || !held {
		return ctx, func() {}, false, err
	}
	lctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(cellLeaseTTL / 4)
		defer t.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-t.C:
				if ok, err := s.Store.AcquireLease(lctx, lease, holder, cellLeaseTTL); err != nil || !ok {
					cancel()
					return
				}
			}
		}
	}()
	return lctx, func() {
		cancel()
		_ = s.Store.DeleteLease(context.WithoutCancel(ctx), lease, holder)
	}, true, nil
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
