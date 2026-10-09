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
	"net/url"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
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

// RunUpstream runs one upstream's matrix (the caller holds its lease): it takes the pending request
// (dry or not), fetches every cell, and records the result. renew keeps the lease.
func (s *Service) RunUpstream(ctx context.Context, id string, renew func(context.Context) error) (Result, error) {
	res := Result{Started: time.Now().UTC(), Counts: map[string]int{}}
	var u store.Upstream
	runnable, requested := false, false
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		if runnable, res.DryRun, requested, err = tx.StartUpstreamRun(ctx, id, s.Config.Interval > 0); err != nil || !runnable {
			return err
		}
		u, err = tx.GetUpstreamByID(ctx, id)
		return err
	})
	if err != nil {
		return res, err
	}
	if !runnable {
		return res, ErrNotDue
	}
	runErr := s.run(ctx, u, &res, renew)
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
	err = s.Store.InTx(fctx, "", func(tx *store.Tx) error {
		if interrupted && requested { // a shutdown or a lost lease: the request stays for another run
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

type cell struct{ version, platform, name string }

func (s *Service) run(ctx context.Context, u store.Upstream, res *Result, renew func(context.Context) error) error {
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
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		last, err = tx.Cells(ctx, u.ID)
		return err
	}); err != nil {
		return err
	}
	// groups: one (name, platform), its cells in ascending DuckDB-version order
	var groups [][]cell
	inMatrix := map[[3]string]bool{}
	for _, e := range u.Entries {
		for _, p := range u.Platforms {
			var g []cell
			for _, v := range versions {
				g = append(g, cell{v, p, e.Name})
				inMatrix[[3]string{v, p, e.Name}] = true
			}
			groups = append(groups, g)
		}
	}
	if !res.DryRun {
		for k := range last {
			if !inMatrix[k] {
				if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeleteCell(ctx, u.ID, k[0], k[1], k[2]) }); err != nil {
					return err
				}
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

	var mu sync.Mutex
	note := func(c cell, outcome, detail string) {
		mu.Lock()
		defer mu.Unlock()
		res.Counts[outcome]++
		if outcome != store.CellReleased && outcome != store.CellUnchanged && outcome != would && len(res.Problems) < maxProblems {
			res.Problems = append(res.Problems, CellNote{c.version, c.platform, c.name, outcome, detail})
		}
	}
	fresh := freshness(s.Store, u.ID)
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
				outcome := s.cell(rctx, actor, sc, cur, e, ks, base, hostGate, c, prev, res.DryRun)
				if rctx.Err() != nil {
					return // a stopped run records nothing more: another run takes the cell
				}
				counted := outcome.Outcome
				if outcome.FetchedAt.Equal(prev.FetchedAt) && prev.Outcome != "" { // a 304: the cell keeps its outcome
					counted = store.CellUnchanged
				}
				note(c, counted, outcome.Detail)
				if !res.DryRun {
					wctx := context.WithoutCancel(ctx)
					if err := s.Store.InTx(wctx, "", func(tx *store.Tx) error {
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
					}); err != nil {
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
	return ctx.Err()
}

// errStopped is a run stopped by a pause.
var errStopped = errors.New("the upstream was paused")

func (s *Service) paused(ctx context.Context, id string) (bool, error) {
	var paused bool
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		u, err := tx.GetUpstreamByID(ctx, id)
		paused = err == nil && u.State != store.UpstreamActive
		return err
	})
	return paused, err
}

// refusal outcomes: a cell turning into one is an upstream.rejected event.
var refusal = map[string]bool{store.CellRejected: true, store.CellConflict: true, store.CellBlocked: true, store.CellShadowed: true}

// conditional outcomes: the same answer changes nothing, so the last ETag is sent.
var conditional = map[string]bool{store.CellReleased: true, store.CellUnchanged: true, store.CellYanked: true, store.CellConflict: true}

// cell fetches and takes in one cell, and returns its record.
func (s *Service) cell(ctx context.Context, a authz.Actor, sc store.ServeChannel, u store.Upstream, e store.UpstreamEntry,
	keys []*rsa.PublicKey, base string, hostGate chan struct{}, c cell, prev store.UpstreamCell, dryRun bool) store.UpstreamCell {
	rec := store.UpstreamCell{UpstreamID: u.ID, DuckDBVersion: c.version, Platform: c.platform, Name: c.name, FetchedAt: time.Now().UTC()}
	fail := func(outcome string, err error) store.UpstreamCell {
		rec.Outcome = outcome
		if err != nil {
			rec.Detail = err.Error()
			if outcome != store.CellMissing {
				s.Log.Info("upstream: a cell was not released", "tenant", sc.Tenant.Name, "upstream", u.Name,
					"duckdb_version", c.version, "platform", c.platform, "name", c.name, "outcome", outcome, "reason", rec.Detail)
			}
		}
		return rec
	}
	etag := ""
	if !dryRun && conditional[prev.Outcome] {
		etag = prev.ETag
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
	if err := acquire(ctx, hostGate); err != nil {
		return fail(store.CellFailed, err)
	}
	url := base + "/" + c.version + "/" + c.platform + "/" + c.name + ".duckdb_extension.gz"
	d, err := s.Fetch.Download(ctx, url, egress.DownloadOptions{ETag: etag, MaxBytes: s.MaxBody + 1<<20, Timeout: timeout, MinRate: rate}, tmp)
	<-hostGate
	switch {
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
	in := release.Ingest{Name: c.name, DuckDBVersion: c.version, Platform: c.platform, Upstream: u.Name, URL: url, Versions: e.Versions, Keys: keys,
		Visibility: u.Visibility, DryRun: dryRun, Provenance: func(key string) string {
			b, _ := json.Marshal(map[string]string{"upstream": u.Name, "kind": u.Kind, "url": url, "etag": d.ETag, "key": key,
				"fetched_at": rec.FetchedAt.Format(time.RFC3339)})
			return string(b)
		}}
	got, err := s.Releases.Ingest(ctx, a, sc, sp, in)
	rec.BodyHash = got.Hash
	switch {
	case errors.Is(err, release.ErrBlocked):
		return fail(store.CellBlocked, err)
	case errors.Is(err, release.ErrShadowed):
		return fail(store.CellShadowed, err)
	case errors.Is(err, release.ErrConflict):
		return fail(store.CellConflict, err)
	case errors.Is(err, store.ErrBusy), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fail(store.CellFailed, err)
	case err != nil:
		return fail(store.CellRejected, err)
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
			u, err = tx.GetUpstreamByID(ctx, id)
			return err
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
	case errors.Is(err, errFailed):
		return "failed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	}
	return "error"
}
