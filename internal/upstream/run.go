package upstream

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// gates bound a replica's fetches: overall, per upstream host, and the ingest slots runs may use.
type gates struct {
	once   sync.Once
	all    chan struct{}
	ingest chan struct{}
	mu     sync.Mutex
	hosts  map[string]chan struct{}
}

func (s *Service) gates() *gates {
	g := &s.gate
	g.once.Do(func() {
		n := s.Config.Concurrency
		if n <= 0 {
			n = 4
		}
		g.all = make(chan struct{}, n)
		g.ingest = make(chan struct{}, max(1, s.MaxIngests/2))
		g.hosts = map[string]chan struct{}{}
	})
	return g
}

func (g *gates) host(h string, n int) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.hosts[h]
	if c == nil {
		c = make(chan struct{}, n)
		g.hosts[h] = c
	}
	return c
}

func acquire(ctx context.Context, c chan struct{}) error {
	select {
	case c <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Result is a run's record (upstreams.last_run).
type Result struct {
	Started  time.Time      `json:"started"`
	Ended    time.Time      `json:"ended"`
	DryRun   bool           `json:"dry_run,omitempty"`
	Error    string         `json:"error,omitempty"` // the run failed before its cells
	Counts   map[string]int `json:"counts"`
	Problems []CellNote     `json:"problems,omitempty"` // the first 20 cells that were not released or unchanged
	Bundle   string         `json:"bundle,omitempty"`   // a bundle import's manifest id (spec 0014)

	actor string // the run's actor, for its event
}

// CellNote is one cell's outcome in a run's record.
type CellNote struct {
	DuckDBVersion string `json:"duckdb_version"`
	Platform      string `json:"platform"`
	Name          string `json:"name"`
	Outcome       string `json:"outcome"`
	Detail        string `json:"detail,omitempty"`
}

const maxProblems = 20

// would is a dry run's outcome for a cell that would be released.
const would = "would_release"

// A lazy upstream's run counts (spec 0032): cells of its matrix nobody asked for, which it does not
// fetch, and cells whose lease another fetch holds, which it skips.
const (
	notAsked = "not_asked"
	busy     = "busy"
)

// RunUpstream runs one upstream's matrix (the caller holds its lease): it takes the pending request
// (dry or not), fetches every cell (a lazy upstream's: the cells asked for and its eager entries,
// every cell when an administrator asked for the run), and records the result. renew keeps the
// lease.
func (s *Service) RunUpstream(ctx context.Context, id string, renew func(context.Context) error) (Result, error) {
	return s.runUpstream(ctx, id, renew, nil)
}

// runUpstream is RunUpstream; only, when set, is the cells a lazy upstream's run fetches besides
// those asked for and its eager entries (a bundle export's plan, spec 0032).
func (s *Service) runUpstream(ctx context.Context, id string, renew func(context.Context) error, only map[[3]string]bool) (Result, error) {
	ctx = store.WithLicenseWarnings(ctx) // spec 0035 phase 2: the run's licence warnings, once per source, name and channel
	res := Result{Started: time.Now().UTC(), Counts: map[string]int{}}
	var u store.Upstream
	runnable, requested := false, false
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		if runnable, res.DryRun, requested, err = tx.StartUpstreamRun(ctx, id, s.Config.Interval > 0); err != nil || !runnable {
			return err
		}
		if u, err = tx.GetUpstreamByID(ctx, id); err != nil {
			return err
		}
		return tx.WritableTenant(ctx, u.TenantID, false) // spec 0019: no run starts for a deleting tenant
	})
	if err != nil {
		return res, err
	}
	if !runnable {
		return res, ErrNotDue
	}
	// a lazy upstream's sync fetches its whole matrix (spec 0032); a dry run covers what a run would
	full := u.FetchMode != store.FetchLazy || requested && !res.DryRun && only == nil
	trigger := triggerRun
	if requested {
		trigger = triggerSync
	}
	runErr := s.run(ctx, u, &res, renew, full, only, trigger)
	if runErr != nil {
		res.Error = runErr.Error()
	}
	res.Ended = time.Now().UTC()
	b, _ := json.Marshal(res)
	fctx := context.WithoutCancel(ctx) // a stopping run still records its result
	interrupted := runErr != nil && !errors.Is(runErr, errStopped) && !errors.Is(runErr, errFailed)
	var next time.Time
	switch {
	case s.Config.Interval <= 0 || u.Mode != store.ModeMirror:
	case interrupted: // a store error or a lost lease: retried after a pause, not at the next poll
		next = res.Ended.Add(retryAfter)
	default: // the next run, with up to 10% jitter
		next = res.Ended.Add(s.Config.Interval + time.Duration(rand.Int64N(int64(s.Config.Interval/10)+1)))
	}
	err = s.Store.InTenantTx(fctx, u.TenantID, nil, func(tx *store.Tx) error {
		// a shutdown or a lost lease: the request stays for another run (a bundle export's of a lazy
		// upstream is its own, which it reports failed)
		if interrupted && requested && only == nil {
			cur, err := tx.GetUpstreamByID(fctx, id)
			if err != nil {
				return err
			}
			if err := tx.RequestUpstreamRun(fctx, &cur, res.DryRun); err != nil {
				return err
			}
		}
		if err := tx.FinishUpstreamRun(fctx, id, string(b), !res.DryRun, res.Started, next); err != nil {
			return err
		}
		who := res.actor
		if who == "" { // the run failed before naming itself
			who = "system:upstream:" + u.Name
			if sc, err := s.Store.ServeChannelByID(fctx, u.ChannelID); err == nil {
				who = "system:upstream:" + sc.Tenant.Name + "/" + u.Name
			}
		}
		return tx.Event(fctx, u.TenantID, who, "upstream.run", "upstream:"+u.Name,
			map[string]any{"upstream": u.Name, "dry_run": res.DryRun, "counts": res.Counts, "error": runClass(runErr)})
	})
	return res, err
}

// retryAfter is how long an interrupted scheduled run waits before it is due again.
const retryAfter = 5 * time.Minute

// ErrNotDue is an upstream that was no longer runnable when its run started: paused, or run by
// another replica meanwhile.
var ErrNotDue = errors.New("upstream: not due")

// errFailed wraps a run's own failure (keys, matrix): the run ended, it was not interrupted.
var errFailed = errors.New("the run failed")

// bodyRoom is a run's (or a lazy fetch's) room for new bodies (spec 0038). Without room (the
// tenant uses its whole bytes limit or the upstream's catalogue is full when it starts, or a cell
// was refused by either since), a cell that holds no body and keeps no ETag is skipped without a
// request; a cell that holds a body runs as ever, and room decides its body.
type bodyRoom struct {
	mu    sync.Mutex
	short error // why there is no room: the start's check, or the first refusal since
	met   error // the first refusal a cell met: the run ends failed
}

// noRoom is why there is no room for a new body, nil while there is.
func (r *bodyRoom) noRoom() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.short
}

// refusal is the first refusal a cell met, nil when none did.
func (r *bodyRoom) refusal() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.met
}

// refuse records a cell's refusal: no room from now on.
func (r *bodyRoom) refuse(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.met == nil {
		r.met = err
	}
	if r.short == nil {
		r.short = err
	}
}

// newBodyRoom is the room a run or a lazy fetch starts with: none when the tenant uses all of its
// bytes limit or the upstream's catalogue is full.
func (s *Service) newBodyRoom(ctx context.Context, u store.Upstream) (*bodyRoom, error) {
	r := &bodyRoom{}
	switch err := s.Releases.NewBodyRoom(ctx, u.TenantID, store.UpstreamSource(u.ID)); {
	case isRoomRefusal(err):
		r.short = err
	case err != nil:
		return nil, err
	}
	return r, nil
}

// atLimit marks a cell skipped for want of room (spec 0038): not requested, its record left as it
// is; a run counts it, never records it.
const atLimit = "skipped_at_limit"

// isRoomRefusal reports a refusal for room (spec 0038): the tenant's limit, the source's full
// catalogue. Neither is the file's fault, and neither clears within a run.
func isRoomRefusal(err error) bool {
	return errors.Is(err, store.ErrTenantLimit) || errors.Is(err, store.ErrCatalogueFull)
}

type cell struct{ version, platform, name string }

func (s *Service) run(ctx context.Context, u store.Upstream, res *Result, renew func(context.Context) error, full bool, only map[[3]string]bool,
	trigger string) error {
	sc, err := s.Store.ServeChannelByID(ctx, u.ChannelID)
	if err != nil {
		return err
	}
	versions, err := s.Store.ChannelVersions(ctx, u.ChannelID)
	if err != nil {
		return err
	}
	slices.SortFunc(versions, release.CompareVersions)
	if n := len(versions) * len(u.Platforms) * len(u.Entries); n > store.MaxUpstreamCells {
		return fmt.Errorf("%w: the matrix is %d cells (at most %d)", errFailed, n, store.MaxUpstreamCells)
	}
	var tenantCells int
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		tenantCells, err = tx.TenantCells(ctx, u.TenantID, "")
		return err
	}); err != nil {
		return err
	}
	if tenantCells > store.MaxTenantCells {
		return fmt.Errorf("%w: the tenant's upstreams would fetch %d cells (at most %d)", errFailed, tenantCells, store.MaxTenantCells)
	}
	keys, err := s.keys(ctx, u)
	if err != nil {
		return fmt.Errorf("%w: %w", errFailed, err)
	}
	fingerprints := make([]string, len(keys))
	for i, k := range keys {
		fingerprints[i] = extfile.Fingerprint(k)
	}
	var last map[[3]string]store.UpstreamCell
	var asks map[[3]string]time.Time
	lazy := u.FetchMode == store.FetchLazy
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		if last, err = tx.Cells(ctx, u.ID); err != nil || !lazy {
			return err
		}
		asks, err = tx.Asks(ctx, u.ID)
		return err
	}); err != nil {
		return err
	}
	// groups: one (name, platform), its cells in ascending DuckDB-version order; a lazy upstream's
	// run takes the cells asked for and its eager entries' (spec 0032)
	var groups [][]cell
	inMatrix := map[[3]string]bool{}
	for _, e := range u.Entries {
		for _, p := range u.Platforms {
			var g []cell
			for _, v := range versions {
				k := [3]string{v, p, e.Name}
				inMatrix[k] = true
				_, asked := asks[k]
				// a bundle export's run takes the plan's cells besides, so the scheduled run it may
				// stand for still keeps the asked cells current
				if !full && !only[k] && !e.Eager && !asked {
					if _, fetched := last[k]; !fetched { // as the cells API lists them
						res.Counts[notAsked]++
					}
					continue
				}
				g = append(g, cell{v, p, e.Name})
			}
			if len(g) > 0 {
				groups = append(groups, g)
			}
		}
	}
	if !res.DryRun {
		gone := func(k [3]string, del func(tx *store.Tx) error) error {
			if inMatrix[k] {
				return nil
			}
			return s.Store.InTenantTx(ctx, u.TenantID, nil, del)
		}
		for k := range last {
			if err := gone(k, func(tx *store.Tx) error { return tx.DeleteCell(ctx, u.ID, k[0], k[1], k[2]) }); err != nil {
				return err
			}
		}
		for k := range asks {
			if err := gone(k, func(tx *store.Tx) error { return tx.DeleteAsk(ctx, u.ID, k[0], k[1], k[2]) }); err != nil {
				return err
			}
		}
	}
	base := s.prefix(u)
	pu, _ := url.Parse(base)
	g := s.gates()
	perHost := s.Config.PerHost
	if perHost <= 0 {
		perHost = 2
	}
	hostGate := g.host(pu.Host, perHost)
	actor := authz.Actor{Kind: authz.ActorSystem, ID: "upstream:" + sc.Tenant.Name + "/" + u.Name}
	res.actor = actor.String()
	// spec 0038: the tenant's use is read once here; the first refusal in a cell stops new downloads
	room, err := s.newBodyRoom(ctx, u)
	if err != nil {
		return err
	}

	var mu sync.Mutex
	note := func(c cell, outcome, detail string) {
		mu.Lock()
		defer mu.Unlock()
		res.Counts[outcome]++
		if outcome != store.CellReleased && outcome != store.CellUnchanged && outcome != store.CellLanded && outcome != store.CellHeld && outcome != would &&
			len(res.Problems) < maxProblems {
			res.Problems = append(res.Problems, CellNote{c.version, c.platform, c.name, outcome, detail})
		}
	}
	fresh := freshness(s.Store, u.ID)
	holder := "run:" + store.NewID() // a lazy upstream's cell leases (spec 0032)
	// the lease is renewed, and a pause noticed, while cells download (one may take fetch_timeout)
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
			}
			if err := renew(rctx); err != nil {
				cancel(err)
				return
			}
			if paused, err := s.paused(rctx, u.ID); err == nil && paused {
				cancel(errStopped)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for _, grp := range groups {
		if acquire(rctx, g.all) != nil {
			break
		}
		wg.Add(1)
		go func(grp []cell) {
			defer wg.Done()
			defer func() { <-g.all }()
			for _, c := range grp {
				if rctx.Err() != nil {
					return
				}
				// each cell follows the upstream as it is now: paused or removed stops the run; a
				// removed entry or platform, a narrowed version list, or an unpinned key applies at once
				cur, err := fresh(rctx)
				if err != nil || cur.State != store.UpstreamActive {
					cancel(errStopped)
					return
				}
				e, ok := entry(cur, c.name)
				if !ok || !slices.Contains(cur.Platforms, c.platform) {
					continue
				}
				ks := keys
				if cur.Kind == store.UpstreamRepo {
					ks = nil
					for i, k := range keys {
						if slices.Contains(cur.Keys, fingerprints[i]) {
							ks = append(ks, k)
						}
					}
				}
				prev := last[[3]string{c.version, c.platform, c.name}]
				cctx, done := rctx, func() {}
				// a lazy upstream's run (or one turned lazy meanwhile) and a first request's fetch
				// never fetch one cell at once
				if (lazy || cur.FetchMode == store.FetchLazy) && !res.DryRun {
					var held bool
					cctx, done, held, err = s.holdCell(rctx, holder, u.ID, c)
					if err == nil && !held {
						mu.Lock()
						res.Counts[busy]++
						mu.Unlock()
						continue
					}
					if err != nil {
						note(c, store.CellFailed, err.Error())
						continue
					}
				}
				outcome := s.cell(cctx, actor, sc, cur, e, ks, base, hostGate, c, prev, res.DryRun, trigger, room)
				lost := cctx.Err() != nil
				done()
				if outcome.Outcome == "" { // the identity provider refuses kista's own client: the run fails
					cancel(fmt.Errorf("%w: %w", errFailed, credential.ErrClient))
					return
				}
				if rctx.Err() != nil {
					return // a stopped run records nothing more: another run takes the cell
				}
				if outcome.Outcome == atLimit { // spec 0038: counted, its record left as it is
					mu.Lock()
					res.Counts[atLimit]++
					mu.Unlock()
					continue
				}
				if lost { // the cell's lease was lost: another fetch takes the cell, and records it
					mu.Lock()
					res.Counts[busy]++
					mu.Unlock()
					continue
				}
				counted := outcome.Outcome
				if outcome.FetchedAt.Equal(prev.FetchedAt) && prev.Outcome != "" { // a 304: the cell keeps its outcome
					counted = store.CellUnchanged
				}
				note(c, counted, outcome.Detail)
				if !res.DryRun {
					wctx := context.WithoutCancel(ctx)
					if err := s.Store.InTenantTx(wctx, u.TenantID, nil, func(tx *store.Tx) error {
						if err := tx.PutCell(wctx, outcome); err != nil {
							return err
						}
						// a cell turning into a refusal is an event (spec 0010); not its text, which may hold URLs
						if refusal[outcome.Outcome] && outcome.Outcome != prev.Outcome {
							return tx.Event(wctx, u.TenantID, actor.String(), "upstream.rejected", "upstream:"+u.Name,
								map[string]any{"upstream": u.Name, "duckdb_version": c.version, "platform": c.platform, "name": c.name,
									"outcome": outcome.Outcome})
						}
						return nil
					}); errors.Is(err, store.ErrDeleting) {
						cancel(errStopped) // spec 0019: the tenant's deletion started
						return
					} else if err != nil {
						s.Log.Warn("upstream: recording a cell", "upstream", u.Name, "error", err)
					}
				}
			}
		}(grp)
	}
	wg.Wait()
	if err := context.Cause(rctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if !lazy && !res.DryRun && ctx.Err() == nil {
		// turned lazy during the run: the cells it recorded since are asked for too
		if err := s.Store.InTenantTx(ctx, u.TenantID, nil, func(tx *store.Tx) error {
			cur, err := tx.GetUpstreamByID(ctx, u.ID)
			if err != nil || cur.FetchMode != store.FetchLazy {
				return err
			}
			return tx.AskFilled(ctx, u.ID)
		}); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := room.refusal(); err != nil { // spec 0038: a cell refused: failed, not interrupted; the next run comes at its time
		return fmt.Errorf("%w: %w", errFailed, err)
	}
	return nil
}

// errStopped is a run stopped by a pause.
var errStopped = errors.New("the upstream was paused")

func (s *Service) paused(ctx context.Context, id string) (bool, error) {
	var paused bool
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		u, err := tx.GetUpstreamByID(ctx, id)
		if err != nil {
			return err
		}
		paused = u.State != store.UpstreamActive
		if werr := tx.WritableTenant(ctx, u.TenantID, false); errors.Is(werr, store.ErrDeleting) {
			paused = true // spec 0019: the tenant's deletion started
		}
		return nil
	})
	return paused, err
}

// refusal outcomes: a cell turning into one is an upstream.rejected event.
var refusal = map[string]bool{store.CellRejected: true, store.CellConflict: true, store.CellBlocked: true, store.CellShadowed: true,
	store.CellOtherSource: true}

// conditional outcomes: the same answer changes nothing, so the last ETag is sent.
var conditional = map[string]bool{store.CellReleased: true, store.CellUnchanged: true, store.CellYanked: true, store.CellConflict: true,
	store.CellLanded: true, store.CellHeld: true}

// What made a cell's fetch, for its upstream.release event (spec 0032): a scheduled run, an
// administrator's sync (a bundle export's too), or a caller's miss (pull-through, a lazy ask).
const (
	triggerRun  = "run"
	triggerSync = "sync"
	triggerAsk  = "ask"
)

// A cell's download answered 503 or 429 is asked again at most fetchRetries times, after a
// Retry-After of at most maxRetryAfter (spec 0032).
const (
	fetchRetries  = 10 // the owner's choice: about 50 seconds at a lazy kista's Retry-After of 5
	maxRetryAfter = 30 * time.Second
)

// cell fetches and takes in one cell, and returns its record. room is the run's room for new bodies
// (spec 0038): a cell meeting the tenant's limit or the source's full catalogue refuses it.
func (s *Service) cell(ctx context.Context, a authz.Actor, sc store.ServeChannel, u store.Upstream, e store.UpstreamEntry,
	keys []*rsa.PublicKey, base string, hostGate chan struct{}, c cell, prev store.UpstreamCell, dryRun bool, trigger string,
	room *bodyRoom) store.UpstreamCell {
	rec := store.UpstreamCell{UpstreamID: u.ID, DuckDBVersion: c.version, Platform: c.platform, Name: c.name, FetchedAt: time.Now().UTC()}
	fail := func(outcome string, err error) store.UpstreamCell {
		rec.Outcome = outcome
		if outcome == store.CellFailed {
			// never the file's fault: the record keeps the body the cell holds (a landed build stays
			// its cell's) and its ETag, as an import's does
			rec.BodyHash, rec.ETag = prev.BodyHash, prev.ETag
		}
		if err != nil {
			rec.Detail = err.Error()
			if outcome != store.CellMissing {
				s.Log.Info("upstream: a cell was not released", "tenant", sc.Tenant.Name, "upstream", u.Name,
					"duckdb_version", c.version, "platform", c.platform, "name", c.name, "outcome", outcome, "reason", rec.Detail)
			}
		}
		return rec
	}
	// spec 0031: a name another source holds in the channel is not fetched
	sel, takes, err := s.Store.SourceTakes(ctx, sc.Channel.ID, c.name, store.UpstreamSource(u.ID))
	if err != nil {
		return fail(store.CellFailed, err)
	} else if !takes {
		label, err := s.Store.SourceLabel(ctx, sel.Source)
		if err != nil {
			return fail(store.CellFailed, err)
		}
		return fail(store.CellOtherSource, &release.SourceError{Name: c.name, Source: label})
	}
	etag := ""
	// a name without a source (cleared) fetches the body again, so the same body sets it
	if !dryRun && conditional[prev.Outcome] && sel.Source != "" {
		etag = prev.ETag
	}
	// spec 0038: without room, a cell that holds no body and keeps no ETag (a dry run's too) is not
	// requested and its record not touched; a cell that holds a body runs as ever, and room decides
	if room.noRoom() != nil && prev.BodyHash == "" && prev.ETag == "" {
		return store.UpstreamCell{Outcome: atLimit}
	}
	// kista serve creates the directory at start; kista admin bundle export -sync runs here without it
	if s.TempDir != "" {
		if err := os.MkdirAll(s.TempDir, 0o700); err != nil {
			return fail(store.CellFailed, err)
		}
	}
	tmp, err := os.CreateTemp(s.TempDir, "kista-upstream-*")
	if err != nil {
		return fail(store.CellFailed, err)
	}
	defer func() {
		tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	timeout := s.Config.FetchTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	rate := s.Config.MinRate
	if rate <= 0 {
		rate = 64 << 10
	}
	// a private upstream's token, before the host's slot (a slow identity provider holds none);
	// its failures are classes in the cell, the details in the log (spec 0009 phase 3)
	cred, tok, err := s.token(ctx, sc.Tenant.Name, u)
	if errors.Is(err, credential.ErrClient) {
		rec.Outcome = "" // the operator's configuration: the run stops, the cell is not recorded
		return rec
	}
	if err != nil {
		return fail(store.CellFailed, err)
	}
	url := base + "/" + c.version + "/" + c.platform + "/" + c.name + ".duckdb_extension.gz"
	opts := egress.DownloadOptions{ETag: etag, MaxBytes: s.MaxBody + 1<<20, Timeout: timeout, MinRate: rate}
	var d egress.Download
	for attempt := 0; ; attempt++ {
		if err := acquire(ctx, hostGate); err != nil {
			return fail(store.CellFailed, err)
		}
		d, tok, err = s.download(ctx, cred, tok, url, opts, tmp)
		<-hostGate
		// spec 0032: a 503 or 429 with a short Retry-After (a lazy upstream fetching the cell) is
		// asked again within the cell, the host's slot free meanwhile
		var re *egress.RetryError
		if !errors.As(err, &re) || !re.Given || re.After > maxRetryAfter || attempt >= fetchRetries {
			break
		}
		if err := tmp.Truncate(0); err != nil {
			return fail(store.CellFailed, err)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return fail(store.CellFailed, err)
		}
		select {
		case <-ctx.Done():
			return fail(store.CellFailed, ctx.Err())
		case <-time.After(re.After):
		}
	}
	var ae *egress.AuthError
	switch {
	case errors.Is(err, credential.ErrClient):
		rec.Outcome = ""
		return rec
	case errors.As(err, &ae):
		return fail(store.CellFailed, fmt.Errorf("upstream.auth: the upstream answered %d", ae.Status))
	case errors.Is(err, credential.ErrToken):
		s.Log.Warn("upstream: no token for a private upstream", "tenant", sc.Tenant.Name, "upstream", u.Name, "error", err)
		return fail(store.CellFailed, errors.New("upstream.auth: no token"))
	case errors.Is(err, egress.ErrNotFound):
		return fail(store.CellMissing, nil)
	case err != nil:
		return fail(store.CellFailed, err)
	case d.NotModified && etag == "":
		return fail(store.CellFailed, errors.New("a 304 to a request that was not conditional"))
	case d.NotModified:
		return prev // its fetched_at unchanged: the run counts it as unchanged
	}
	rec.ETag = d.ETag
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fail(store.CellFailed, err)
	}
	body, err := inflate(tmp)
	if err != nil {
		return fail(store.CellRejected, err)
	}
	g := s.gates()
	if err := acquire(ctx, g.ingest); err != nil {
		return fail(store.CellFailed, err)
	}
	defer func() { <-g.ingest }()
	sp, err := s.Blob.Spool(ctx, sc.Tenant.StorageDomain, body)
	if err != nil {
		if isIOError(err) && !errors.Is(err, blob.ErrRead) || ctx.Err() != nil {
			return fail(store.CellFailed, err) // the spool's disk, or the run stopping
		}
		return fail(store.CellRejected, err) // the file: too large, malformed, a broken gzip stream
	}
	defer sp.Close()
	vis := u.Visibility
	if u.Credential != "" && (cred == nil || !cred.AllowPublic) {
		vis = store.Private // whatever the upstream's visibility says (spec 0009 phase 3)
	}
	in := release.Ingest{Name: c.name, DuckDBVersion: c.version, Platform: c.platform, Upstream: u.Name, UpstreamID: u.ID, URL: url, Versions: e.Versions, Keys: keys,
		Trigger:    trigger,
		Visibility: vis, DryRun: dryRun, Provenance: func(key string) string {
			p := map[string]string{"upstream": u.Name, "kind": u.Kind, "url": url, "etag": d.ETag, "key": key,
				"fetched_at": rec.FetchedAt.Format(time.RFC3339)}
			if u.Credential != "" {
				p["credential"] = u.Credential
			}
			b, _ := json.Marshal(p)
			return string(b)
		}}
	if u.Kind == store.UpstreamRepo {
		in.KeySource = u.ID // spec 0021: a repository upstream is a trusted source of licence keys
	}
	in.ReserveKind = reserveKind(u.Kind) // spec 0030: DuckDB's repositories bring their names
	if !slices.ContainsFunc(u.Entries, func(x store.UpstreamEntry) bool { return x.Name == c.name }) {
		in.AnyKind = u.Kind // "*" took it: checked again under the insert's locks
	}
	in.ReserveSource = "upstream:" + sc.Tenant.Name + "/" + u.Name
	got, err := s.Releases.Ingest(ctx, a, sc, sp, in)
	rec.BodyHash = got.Hash
	switch {
	case errors.Is(err, release.ErrBlocked):
		return fail(store.CellBlocked, err)
	case errors.Is(err, release.ErrShadowed):
		return fail(store.CellShadowed, err)
	case errors.Is(err, release.ErrOtherSource):
		return fail(store.CellOtherSource, err)
	case errors.Is(err, release.ErrConflict):
		return fail(store.CellConflict, err)
	case isRoomRefusal(err): // spec 0038: never the file's fault; new downloads stop
		room.refuse(err)
		return fail(store.CellFailed, err)
	case errors.Is(err, store.ErrBusy), errors.Is(err, store.ErrBuildGone), errors.Is(err, store.ErrStreamBusy),
		errors.Is(err, release.ErrUsage), // spec 0038: the catalogue's use could not be read
		errors.Is(err, store.ErrClaimLost), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, store.ErrSourceGone),
		errors.Is(err, store.ErrDeleting): // spec 0019: never the file's fault; recording the cell stops the run
		return fail(store.CellFailed, err) // tried again by the next run: never the file's fault
	case err != nil:
		return fail(store.CellRejected, err)
	case got.Held:
		rec.Outcome = store.CellHeld
	case got.Landed:
		rec.Outcome = store.CellLanded
	case got.Existed && got.Release.State == store.ReleaseYanked:
		rec.Outcome = store.CellYanked
	case got.Existed:
		rec.Outcome = store.CellUnchanged
	case dryRun:
		rec.Outcome = would
	default:
		rec.Outcome = store.CellReleased
	}
	return rec
}

// inflate returns the file a download holds: a gzip stream (by its magic bytes) inflated, or the
// bytes as they are. The spool bounds the size.
func inflate(f *os.File) (io.Reader, error) {
	br := bufio.NewReader(f)
	magic, err := br.Peek(2)
	if err != nil {
		return nil, fmt.Errorf("upstream: an empty or short file")
	}
	if magic[0] != 0x1f || magic[1] != 0x8b {
		return br, nil
	}
	zr, err := gzip.NewReader(br)
	if err != nil {
		return nil, fmt.Errorf("upstream: a malformed gzip file: %w", err)
	}
	zr.Multistream(false)
	return zr, nil
}

// isIOError tells a local failure (the spool's disk) from a file's fault.
func isIOError(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe)
}

// freshness reads an upstream as it is now, at most every two seconds.
func freshness(st *store.Store, id string) func(context.Context) (store.Upstream, error) {
	var mu sync.Mutex
	var u store.Upstream
	var at time.Time
	return func(ctx context.Context) (store.Upstream, error) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) < 2*time.Second {
			return u, nil
		}
		err := st.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			if u, err = tx.GetUpstreamByID(ctx, id); err != nil {
				return err
			}
			return tx.WritableTenant(ctx, u.TenantID, false) // spec 0019: a deleting tenant's run stops
		})
		if err != nil {
			return u, err
		}
		at = time.Now()
		return u, nil
	}
}

func entry(u store.Upstream, name string) (store.UpstreamEntry, bool) {
	for _, e := range u.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return store.UpstreamEntry{}, false
}

// runClass is a stopped run's error class for its event (never the error's text).
func runClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errStopped):
		return "paused"
	case errors.Is(err, ErrNoKeys):
		return "keys"
	case errors.Is(err, credential.ErrClient):
		return "credential"
	case errors.Is(err, errFailed):
		return "failed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	}
	return "error"
}

// token is the credential an upstream uses (nil for none) and a token of it.
func (s *Service) token(ctx context.Context, tenant string, u store.Upstream) (*credential.Credential, string, error) {
	if u.Credential == "" {
		return nil, "", nil
	}
	c, err := s.Credentials.For(u.Credential, tenant, u.Prefix)
	if err != nil {
		s.Log.Warn("upstream: a private upstream's credential is not configured for it", "tenant", tenant, "upstream", u.Name,
			"credential", u.Credential, "error", err)
		return nil, "", errors.New("upstream.auth: the credential is not configured for this upstream")
	}
	tok, err := c.Token(ctx)
	switch {
	case errors.Is(err, credential.ErrClient):
		s.Log.Error("upstream: the identity provider refuses kista's own client; check the credential's configuration",
			"tenant", tenant, "upstream", u.Name, "credential", u.Credential)
		return nil, "", err
	case err != nil:
		s.Log.Warn("upstream: no token for a private upstream", "tenant", tenant, "upstream", u.Name, "error", err)
		return nil, "", errors.New("upstream.auth: no token")
	}
	return c, tok, nil
}

// download fetches a cell's file, with the credential's token when there is one: a 401 forgets
// that token (unless it is younger than 30 seconds) and tries once more, the file truncated first.
// It answers the token it used last, for a later attempt.
func (s *Service) download(ctx context.Context, cred *credential.Credential, tok, url string, o egress.DownloadOptions, f *os.File) (egress.Download, string, error) {
	for attempt := 0; ; attempt++ {
		if cred != nil {
			o.Header = http.Header{"Authorization": {"Bearer " + tok}}
		}
		d, err := s.Fetch.Download(ctx, url, o, f)
		var ae *egress.AuthError
		if cred == nil || attempt > 0 || !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
			return d, tok, err
		}
		cred.Invalidate(tok)
		if tok, err = cred.Token(ctx); err != nil {
			return d, tok, err
		}
		if err := f.Truncate(0); err != nil {
			return d, tok, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return d, tok, err
		}
	}
}

// reserveKind is the kind a build mirrored from an upstream of kind reserves its name as (spec
// 0030): DuckDB's core repository's are core, its community repository's community; others none.
func reserveKind(kind string) string {
	switch kind {
	case store.UpstreamCore:
		return reserved.Core
	case store.UpstreamCommunity:
		return reserved.Community
	}
	return ""
}
