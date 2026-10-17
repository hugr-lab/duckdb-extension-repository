package upstream

import (
	"cmp"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// ImportOptions are a bundle import's (spec 0014).
type ImportOptions struct {
	Upstreams []string // the targets by name; none: every active bundle-mode upstream
	// Feeds are the feed targets by name; none: every active bundle-mode feed (phase 1b). With only
	// one of Upstreams and Feeds, the other kind takes no target.
	Feeds      []string
	DryRun     bool
	AnyTenant  bool     // import a bundle whose plan names another tenant
	BundleKeys []string // the fingerprints pinned for the tenant (upstreams.bundle_keys)
	Holder     string   // the leases' holder
	// Idle and Floor end a stalled input: nothing for Idle (default 5m), or less than MinRate once
	// Floor (default 30s) of waiting has passed. Tests shorten them.
	Idle, Floor time.Duration
}

// Target statuses in an import's result.
const (
	TargetImported = "imported" // its cells were taken in (some may have been refused)
	TargetBusy     = "busy"     // a run or another import held its lease
	TargetStale    = "stale"    // the bundle is older than the last one it imported, or than the upstream
	TargetFailed   = "failed"   // keys, matrix, a pause, or the bundle ended before its files
)

// TargetResult is one upstream's or feed's part of an import.
type TargetResult struct {
	Upstream string                `json:"upstream,omitempty"`
	Feed     string                `json:"feed,omitempty"` // phase 1b
	Status   string                `json:"status"`
	Detail   string                `json:"detail,omitempty"`
	Result   Result                `json:"run"`
	FeedRun  *attachment.RunResult `json:"feed_run,omitempty"`
}

// ImportResult is an import's outcome: its record, each target's, and what the bundle held that
// no target took.
type ImportResult struct {
	Import    store.BundleImport `json:"-"`
	Targets   []TargetResult     `json:"targets"`
	Unplanned int                `json:"unplanned"`         // bundle cells outside every target's matrix
	Feeds     int                `json:"feeds"`             // feeds the bundle carries that no target took (phase 1b)
	Gaps      int                `json:"gaps"`              // feed bodies whose range did not start at the feed's cursor: write a new plan
	Skipped   int64              `json:"skipped_bytes"`     // entries read through
	Problem   string             `json:"problem,omitempty"` // why the import stopped early
	Stop      string             `json:"stop,omitempty"`    // its class: bundle (truncated, malformed, stalled), interrupted, deleting
}

// Errors of an import.
var (
	// ErrBundleTenant is a bundle planned for another tenant (-any-tenant imports it).
	ErrBundleTenant = errors.New("upstream: the bundle was planned for another tenant")
	// errStall is an input that delivered nothing, or too little, for too long.
	errStall = errors.New("upstream: the bundle's input stalled")
)

type target struct {
	u       store.Upstream
	src     *bundle.Source
	sc      store.ServeChannel
	lease   string
	held    bool
	status  string
	detail  string
	res     Result
	runErr  error
	matrix  map[[3]string]bool
	last    map[[3]string]store.UpstreamCell
	keys    []*rsa.PublicKey
	fps     []string
	keyed   bool // keys known (core, community; a repository once its .well-known entry was read)
	done    map[[3]string]bool
	fresh   func(context.Context) (store.Upstream, error)
	stopped bool
}

func (tg *target) active() bool { return tg.status == TargetImported && !tg.stopped }

// Import takes a bundle into a tenant's bundle-mode upstreams (spec 0014): the head checked
// against the pinned bundle keys, the targets leased and checked for freshness, then every file
// once, front to back, into each target cell it fills, as a run takes in a fetched file. Releases
// become current as they are inserted, in the bundle's canonical order, as in a run. The returned
// error is a refusal before anything was taken in (the bundle, the tenant, the targets);
// afterwards problems are in the result and the record. Importing is a server administrator's act.
func (s *Service) Import(ctx context.Context, a authz.Actor, tenant string, in io.Reader, o ImportOptions) (ImportResult, error) {
	var out ImportResult
	t, err := s.serverTenant(ctx, a, tenant)
	if err != nil {
		return out, err
	}
	if o.Idle <= 0 {
		o.Idle = 5 * time.Minute
	}
	if o.Floor <= 0 {
		o.Floor = 30 * time.Second
	}
	ictx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	rate := s.Config.MinRate
	if rate <= 0 {
		rate = 64 << 10
	}
	w := watch(ictx, in, o.Idle, o.Floor, rate)
	defer w.stop()
	b, err := bundle.Open(w, s.Store.Now(), func(fp string) bool { return slices.Contains(o.BundleKeys, fp) })
	if err != nil {
		return out, err
	}
	m := b.Manifest
	if m.Plan.Tenant != t.Name && !o.AnyTenant {
		return out, fmt.Errorf("%w: %s, not %s (-any-tenant imports it)", ErrBundleTenant, m.Plan.Tenant, t.Name)
	}
	var targets []*target
	if len(o.Upstreams) > 0 || len(o.Feeds) == 0 {
		if targets, err = s.importTargets(ctx, t, m, o.Upstreams); err != nil {
			return out, err
		}
	}
	var fts []*ftarget
	if len(o.Feeds) > 0 || len(o.Upstreams) == 0 {
		if fts, err = s.feedTargets(ctx, t, m, o.Feeds); err != nil {
			return out, err
		}
	}
	if len(targets) == 0 && len(fts) == 0 {
		return out, fmt.Errorf("%w: no active bundle-mode upstream or feed of tenant %s has a source or feed in the bundle", store.ErrInvalid, t.Name)
	}
	if err := s.failInterrupted(ctx, t.ID); err != nil {
		return out, err
	}
	// the record's lease before the record: another import never takes it for an interrupted one
	rec := store.BundleImport{ID: store.NewID(), TenantID: t.ID, ManifestID: m.ID, ManifestCreatedAt: m.CreatedAt,
		ManifestSHA256: b.ManifestSHA, Exporter: exporterText(m.Exporter), BundleKey: b.Fingerprint, DryRun: o.DryRun, Actor: a.String()}
	leases := []string{store.ImportLease(rec.ID)}
	if ok, err := s.Store.AcquireLease(ctx, leases[0], o.Holder, leaseTTL); err != nil || !ok {
		return out, cmp.Or(err, errors.New("upstream: the import's lease is taken"))
	}
	defer func() { _ = s.Store.DeleteLease(context.WithoutCancel(ctx), leases[0], o.Holder) }()
	if err := s.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error { return tx.InsertBundleImport(ctx, &rec) }); err != nil {
		return out, err
	}
	// leases first, then the tenant writable, then each target's freshness and mark
	started := time.Now().UTC()
	var leaseErr error
	for _, tg := range targets {
		tg.res = Result{Started: started, DryRun: o.DryRun, Counts: map[string]int{}, Bundle: m.ID, actor: a.String()}
		if leaseErr != nil {
			continue
		}
		held, err := s.Store.AcquireLease(ctx, tg.lease, o.Holder, leaseTTL)
		switch {
		case err != nil:
			leaseErr = err
		case !held:
			tg.status = TargetBusy
		default:
			tg.held = true
			leases = append(leases, tg.lease)
		}
	}
	for _, ft := range fts { // a feed's lease is its runs': an online run and an import exclude each other
		if leaseErr != nil {
			continue
		}
		held, err := s.Store.AcquireLease(ctx, ft.lease, o.Holder, leaseTTL)
		switch {
		case err != nil:
			leaseErr = err
		case !held:
			ft.status = TargetBusy
		default:
			ft.held = true
			leases = append(leases, ft.lease)
		}
	}
	// the leases are renewed while the import runs; the renewal ends before they are released
	var renewal sync.WaitGroup
	renewal.Add(1)
	go func() {
		defer renewal.Done()
		tk := time.NewTicker(30 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ictx.Done():
				return
			case <-tk.C:
			}
			for _, l := range leases {
				if ok, err := s.Store.RenewLease(ictx, l, o.Holder, leaseTTL); err != nil || !ok {
					if ictx.Err() == nil {
						cancel(errLeaseLost)
					}
					return
				}
			}
		}
	}()
	defer func() {
		cancel(nil)
		renewal.Wait()
		s.releaseLeases(ctx, targets, o.Holder)
		for _, ft := range fts {
			if ft.held {
				_ = s.Store.ReleaseLease(context.WithoutCancel(ctx), ft.lease, o.Holder)
			}
		}
	}()
	if leaseErr == nil {
		leaseErr = s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.WritableTenant(ctx, t.ID, false) })
	}
	if leaseErr != nil {
		s.finishImport(ctx, a, t, &rec, b, &out, targets, fts, leaseErr)
		return out, leaseErr
	}
	for _, tg := range targets {
		if tg.held {
			s.prepare(ctx, tg, m, b.ManifestSHA, o.DryRun)
		}
	}
	defer s.feedSpools(fts)()
	for _, ft := range fts {
		if ft.held {
			s.prepareFeed(ctx, t, ft, m, b.ManifestSHA, o.DryRun)
		}
	}
	out.Unplanned = unplanned(m, targets)
	for fi := range m.Feeds {
		if !slices.ContainsFunc(fts, func(ft *ftarget) bool { return ft.fi == fi }) {
			out.Feeds++
		}
	}
	streamErr := s.stream(ictx, a, t, b, targets, fts, o.DryRun)
	if cause := context.Cause(ictx); cause != nil && !errors.Is(cause, context.Canceled) {
		streamErr = cause // the watchdog, a lost lease
	} else if streamErr == nil && ctx.Err() != nil {
		streamErr = ctx.Err()
	}
	// a stopped import records nothing more on its cells: a lost lease means another holder may be
	// running the upstream, and a deleting tenant takes no writes
	if stopClass(streamErr) == "bundle" || streamErr == nil {
		s.leftovers(ictx, t, targets, o.DryRun, b.Ended())
	}
	// a feed's cursors move only after a whole read; a stopped import moves none
	s.finishFeeds(ctx, a, b, fts, streamErr != nil)
	s.finishImport(ctx, a, t, &rec, b, &out, targets, fts, streamErr)
	return out, nil
}

// errLeaseLost stops an import whose lease another holder took.
var errLeaseLost = errors.New("upstream: an import's lease was lost")

// stopClass names why an import stopped early: "deleting" (the tenant's deletion started),
// "interrupted" (a lost lease, a signal), "bundle" (the bundle was truncated, malformed or stalled).
func stopClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, store.ErrDeleting):
		return "deleting"
	case errors.Is(err, errLeaseLost), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	}
	return "bundle"
}

// exporterText is the manifest's exporter as the record keeps it (at most 1,000 bytes of JSON).
func exporterText(e bundle.Exporter) string {
	for _, n := range []int{bundle.MaxExporter, 200} {
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(bundle.Exporter{Version: bundle.Text(e.Version, n), PublicURL: bundle.Text(e.PublicURL, n)})
		if out := strings.TrimSpace(buf.String()); len(out) <= 1000 {
			return out
		}
	}
	return "{}"
}

// serverTenant authorizes a server administrator's act on a tenant.
func (s *Service) serverTenant(ctx context.Context, a authz.Actor, name string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, name)
}

// importTargets selects the tenant's bundle-mode upstreams the import fills: the named ones (each
// must be in bundle mode, active, and have a source in the bundle), or every active one with a
// source in the bundle. None is a refusal.
func (s *Service) importTargets(ctx context.Context, t store.Tenant, m *bundle.Manifest, names []string) ([]*target, error) {
	var all []store.Upstream
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		all, err = tx.ListUpstreams(ctx, t.ID)
		return err
	}); err != nil {
		return nil, err
	}
	source := func(u store.Upstream) int {
		return slices.IndexFunc(m.Sources, func(src bundle.Source) bool {
			return src.Kind == u.Kind && (u.Kind != store.UpstreamRepo || src.Prefix == u.Prefix)
		})
	}
	for _, n := range names {
		i := slices.IndexFunc(all, func(u store.Upstream) bool { return u.Name == n })
		switch {
		case i < 0:
			return nil, fmt.Errorf("%w: upstream %s", store.ErrNotFound, n)
		case all[i].Mode != store.ModeBundle:
			return nil, fmt.Errorf("%w: upstream %s is not in bundle mode", store.ErrInvalid, n)
		case all[i].State != store.UpstreamActive:
			return nil, fmt.Errorf("%w: upstream %s is paused", store.ErrInvalid, n)
		case source(all[i]) < 0:
			return nil, fmt.Errorf("%w: the bundle holds no source of upstream %s", store.ErrInvalid, n)
		}
	}
	var out []*target
	for _, u := range all {
		if u.Mode != store.ModeBundle || u.State != store.UpstreamActive || len(names) > 0 && !slices.Contains(names, u.Name) {
			continue
		}
		i := source(u)
		if i < 0 {
			continue // an upstream without a source in the bundle is not touched
		}
		tg := &target{u: u, src: &m.Sources[i], lease: "kista/upstream/" + u.ID, status: TargetImported,
			done: map[[3]string]bool{}, fresh: freshness(s.Store, u.ID)}
		var err error
		if tg.matrix, err = s.matrix(ctx, u); err != nil {
			return nil, err
		}
		out = append(out, tg)
	}
	return out, nil // none is the caller's to refuse: the bundle may serve feeds only
}

// matrix is an upstream's cells as a run computes them; nil when it is over a cap (the target
// then fails, as a run fails).
func (s *Service) matrix(ctx context.Context, u store.Upstream) (map[[3]string]bool, error) {
	versions, err := s.Store.ChannelVersions(ctx, u.ChannelID)
	if err != nil {
		return nil, err
	}
	if len(versions)*len(u.Platforms)*len(u.Entries) > store.MaxUpstreamCells {
		return nil, nil
	}
	out := map[[3]string]bool{}
	for _, v := range versions {
		for _, p := range u.Platforms {
			for _, e := range u.Entries {
				out[[3]string{v, p, e.Name}] = true
			}
		}
	}
	return out, nil
}

// prepare checks a leased target: its caps and cells, its keys (core and community; a
// repository's come with the bundle), then its freshness against the bundle, recording the mark
// unless dry. A target that fails a check never records the mark.
func (s *Service) prepare(ctx context.Context, tg *target, m *bundle.Manifest, manifestSHA string, dryRun bool) {
	fail := func(status string, err error) {
		tg.status, tg.detail, tg.runErr = status, err.Error(), err
	}
	var err error
	if tg.sc, err = s.Store.ServeChannelByID(ctx, tg.u.ChannelID); err != nil {
		fail(TargetFailed, err)
		return
	}
	if tg.matrix == nil {
		fail(TargetFailed, fmt.Errorf("%w: the matrix is over %d cells", errFailed, store.MaxUpstreamCells))
		return
	}
	var tenantCells int
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		if tenantCells, err = tx.TenantCells(ctx, tg.u.TenantID, ""); err != nil {
			return err
		}
		tg.last, err = tx.Cells(ctx, tg.u.ID)
		return err
	})
	switch {
	case err != nil:
		fail(TargetFailed, err)
		return
	case tenantCells > store.MaxTenantCells:
		fail(TargetFailed, fmt.Errorf("%w: the tenant's upstreams would fetch %d cells (at most %d)", errFailed, tenantCells, store.MaxTenantCells))
		return
	}
	if tg.u.Kind != store.UpstreamRepo {
		if tg.keys, err = s.keys(ctx, tg.u); err != nil {
			fail(TargetFailed, fmt.Errorf("%w: %w", errFailed, err))
			return
		}
		tg.keyed = true
	}
	// the store keeps microseconds: bundles are ordered at that precision
	created := m.CreatedAt.Truncate(time.Microsecond)
	stale := ""
	err = s.Store.InTenantTx(ctx, tg.u.TenantID, nil, func(tx *store.Tx) error {
		u, err := tx.GetUpstreamByID(ctx, tg.u.ID)
		if err != nil {
			return err
		}
		switch {
		case u.State != store.UpstreamActive || u.Mode != store.ModeBundle:
			return errStopped
		case m.Plan.CreatedAt.Before(u.CreatedAt):
			stale = "the bundle's plan is older than the upstream"
		case u.BundleID == m.ID && u.BundleManifestSHA256 != manifestSHA:
			stale = "a bundle of the same id with another manifest was imported"
		case u.BundleID != "" && u.BundleID != m.ID && !after(created, m.ID, u.BundleCreatedAt.Truncate(time.Microsecond), u.BundleID):
			stale = "a newer bundle was imported (" + u.BundleID + ")"
		}
		tg.u = u
		if stale != "" || dryRun {
			return nil
		}
		// before the first release: an interrupted import still closes the door on older bundles
		return tx.SetBundleMark(ctx, u.ID, m.ID, created, manifestSHA)
	})
	switch {
	case err != nil:
		fail(TargetFailed, err)
	case stale != "":
		fail(TargetStale, fmt.Errorf("upstream.stale: %s", stale))
	}
}

// after reports whether bundle (at, id) is ordered after (bAt, bID): by created_at, then id.
func after(at time.Time, id string, bAt time.Time, bID string) bool {
	if !at.Equal(bAt) {
		return at.After(bAt)
	}
	return id > bID
}

// wellKnown takes a repository source's .well-known entry: the keys whose fingerprints each of its
// targets pinned. A target with none fills nothing.
func wellKnown(targets []*target, src *bundle.Source, data []byte) {
	var wk struct {
		Keys []string `json:"signature_keys"`
	}
	parsed := json.Unmarshal(data, &wk) == nil
	for _, tg := range targets {
		if tg.src != src || !tg.active() || tg.keyed {
			continue
		}
		tg.keyed = true
		for _, k := range wk.Keys {
			pub, err := extfile.ParsePublicKey(k)
			if err != nil {
				continue
			}
			if fp := extfile.Fingerprint(pub); slices.Contains(tg.u.Keys, fp) {
				tg.keys, tg.fps = append(tg.keys, pub), append(tg.fps, fp)
			}
		}
		if !parsed || len(tg.keys) == 0 {
			tg.status, tg.detail = TargetFailed, "upstream.keys: the bundle's keys list none of the pinned fingerprints"
			tg.runErr = fmt.Errorf("%w: %w", errFailed, ErrNoKeys)
		}
	}
}

// stream reads the bundle's items in order and takes each file into the target cells it fills. It
// returns the bundle's own failure (truncated, malformed, stalled), or a stop.
func (s *Service) stream(ctx context.Context, a authz.Actor, t store.Tenant, b *bundle.Bundle, targets []*target, fts []*ftarget, dryRun bool) error {
	byFile := b.Manifest.CellsByFile()
	for {
		it, err := b.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if it.WellKnown != nil {
			wellKnown(targets, it.WellKnown, it.Data)
			continue
		}
		if it.Feed != nil { // statements, after the files (phase 1b)
			if err := s.feedEntry(ctx, it, fts); err != nil {
				return err
			}
			continue
		}
		// a repository target whose .well-known entry never came before the files fills nothing
		for _, tg := range targets {
			if tg.active() && !tg.keyed {
				tg.keyed = true
				tg.status, tg.detail = TargetFailed, "upstream.keys: the bundle carries no .well-known file for the source"
				tg.runErr = fmt.Errorf("%w: %w", errFailed, ErrNoKeys)
			}
		}
		if err := s.file(ctx, a, t, it, byFile[it.File], targets, dryRun); err != nil {
			return err
		}
	}
}

// errSpoolStopped closes a file's pipe once the spool stopped reading it.
var errSpoolStopped = errors.New("upstream: the spool stopped reading")

// inputErr remembers a failure of the bundle's input itself (not of the file's gzip stream).
type inputErr struct {
	r   io.Reader
	err error
}

func (e *inputErr) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

type work struct {
	tg  *target
	ref bundle.Ref
}

// file takes one file into every target cell it fills: inflated into the spool once, its hash
// checked, then each cell as a run takes in a fetched file.
func (s *Service) file(ctx context.Context, a authz.Actor, t store.Tenant, it bundle.Item, refs []bundle.Ref, targets []*target, dryRun bool) error {
	var todo []work
	for _, r := range refs {
		k := [3]string{r.Cell.DuckDBVersion, r.Cell.Platform, r.Cell.Name}
		for _, tg := range targets {
			if tg.active() && tg.src == r.Source && tg.matrix[k] {
				todo = append(todo, work{tg, r})
			}
		}
	}
	if len(todo) == 0 {
		return nil // read through by the next Next
	}
	c0 := todo[0].ref.Cell
	// refuse records the file's cells; only a deleting tenant comes back, and it stops the import
	refuse := func(outcome string, err error) error {
		for _, w := range todo {
			if rerr := s.record(ctx, t, w.tg, w.ref.Cell, store.UpstreamCell{Outcome: outcome, Detail: err.Error()}, dryRun); rerr != nil {
				return rerr
			}
		}
		return nil
	}
	if c0.InflatedSize > s.MaxBody+extfile.SignatureSize {
		return refuse(store.CellRejected, fmt.Errorf("bundle: the file is over blob.max_body"))
	}
	// inflated straight into the spool through a SHA-256 tee: no other temporary copy
	src := &inputErr{r: it.R}
	pr, pw := io.Pipe()
	type inflated struct {
		n   int64
		sum string
		err error
	}
	done := make(chan inflated, 1)
	go func() {
		n, sum, err := bundle.Inflate(src, c0.InflatedSize, pw)
		pw.CloseWithError(err)
		done <- inflated{n, sum, err}
	}()
	sp, err := s.Blob.Spool(ctx, t.StorageDomain, pr)
	pr.CloseWithError(errSpoolStopped) // the inflater never blocks on a spool that stopped reading
	inf := <-done
	if sp != nil {
		defer sp.Close()
	}
	if errors.Is(inf.err, errSpoolStopped) {
		inf.err = nil // the spool's own failure decides
	}
	// the bytes of the gzip entry and the inflated file's hash are not signed: a mismatch is the
	// media's fault, recorded failed (which keeps the cell's record); what the signed hash vouches
	// for and the file checks refuse is rejected
	switch {
	case src.err != nil || inf.err != nil && ctx.Err() != nil:
		// the input ended, broke or stalled inside the file: the bundle stops here, and the file's
		// cells are recorded with the leftovers (a stopped import records nothing more)
		return cmp.Or(src.err, context.Cause(ctx))
	case inf.err != nil:
		return refuse(store.CellFailed, fmt.Errorf("bundle: the gzip entry is damaged: %w", inf.err))
	case err != nil && (isIOError(err) && !errors.Is(err, blob.ErrRead) || ctx.Err() != nil):
		return refuse(store.CellFailed, err) // the spool's disk, or the import stopping
	case inf.n != c0.InflatedSize || inf.sum != c0.SHA256:
		return refuse(store.CellFailed, errors.New("bundle: the file differs from the manifest's size or hash (damaged media)"))
	case err != nil:
		return refuse(store.CellRejected, err)
	}
	for _, w := range todo {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err := s.importCell(ctx, a, t, w.tg, w.ref, sp, dryRun); err != nil {
			return err
		}
	}
	return nil
}

// importCell takes the spooled file into one target cell.
func (s *Service) importCell(ctx context.Context, a authz.Actor, t store.Tenant, tg *target, r bundle.Ref, sp *blob.Spool, dryRun bool) error {
	if !tg.active() {
		return nil
	}
	c := r.Cell
	k := [3]string{c.DuckDBVersion, c.Platform, c.Name}
	// the target as it is now: paused or removed stops it; a removed entry or platform, or an
	// unpinned key, applies to this cell
	cur, err := tg.fresh(ctx)
	switch {
	case errors.Is(err, store.ErrDeleting) || err != nil && ctx.Err() != nil:
		return cmp.Or(context.Cause(ctx), err) // the tenant's deletion started (spec 0019), or the import stops
	case err != nil:
		tg.stopped, tg.runErr = true, err // the store: the target stops, not as a pause
		return nil
	case cur.State != store.UpstreamActive:
		tg.stopped, tg.runErr = true, errStopped
		return nil
	}
	e, ok := entry(cur, c.Name)
	if !ok || !slices.Contains(cur.Platforms, c.Platform) {
		return nil
	}
	keys := tg.keys
	if cur.Kind == store.UpstreamRepo {
		keys = nil
		for i, key := range tg.keys {
			if slices.Contains(cur.Keys, tg.fps[i]) {
				keys = append(keys, key)
			}
		}
	}
	tg.done[k] = true
	prev := tg.last[k]
	// no downgrade: a cell that held another body takes this one only if it was fetched later (the
	// record keeps the newest body and its fetch time through missing, rejected and failed outcomes)
	if prev.BodyHash != "" && prev.BodyHash != c.BodyHash && !c.FetchedAt.After(prev.FetchedAt) {
		tg.note(c, "stale", "the site holds a body fetched later")
		return nil
	}
	if f := sp.File(); f.Hash.String() != c.BodyHash {
		return s.record(ctx, t, tg, c, store.UpstreamCell{Outcome: store.CellRejected, Detail: "bundle: the body hash differs from the manifest's"}, dryRun)
	}
	u := tg.u
	base := s.prefix(u)
	url := base + "/" + c.DuckDBVersion + "/" + c.Platform + "/" + c.Name + ".duckdb_extension.gz"
	vis, cred := u.Visibility, ""
	if c.Authenticated {
		cred = credential.BundleCredential + base
		if s.Releases.MayPublish == nil || !s.Releases.MayPublish(t.Name, cred) {
			vis = store.Private // a subscription's extensions are not the tenant's to publish (spec 0014)
		}
	}
	importedAt := s.Store.Now().UTC().Format(time.RFC3339)
	in := release.Ingest{Name: c.Name, DuckDBVersion: c.DuckDBVersion, Platform: c.Platform, Upstream: u.Name, URL: url,
		Versions: e.Versions, Keys: keys, Visibility: vis, DryRun: dryRun, Bundle: tg.res.Bundle,
		Provenance: func(key string) string {
			p := map[string]string{"upstream": u.Name, "kind": u.Kind, "url": url, "etag": c.ETag, "key": key,
				"fetched_at": c.FetchedAt.UTC().Format(time.RFC3339), "bundle": tg.res.Bundle, "imported_at": importedAt}
			if cred != "" {
				p["credential"] = cred
			}
			b, _ := json.Marshal(p)
			return string(b)
		}}
	got, err := s.Releases.Ingest(ctx, a, tg.sc, sp, in)
	rec := store.UpstreamCell{BodyHash: got.Hash}
	switch {
	case errors.Is(err, store.ErrDeleting):
		return err
	case errors.Is(err, release.ErrBlocked):
		rec.Outcome, rec.Detail = store.CellBlocked, err.Error()
	case errors.Is(err, release.ErrShadowed):
		rec.Outcome, rec.Detail = store.CellShadowed, err.Error()
	case errors.Is(err, release.ErrConflict):
		rec.Outcome, rec.Detail = store.CellConflict, err.Error()
	case errors.Is(err, store.ErrBusy), errors.Is(err, store.ErrBuildGone), errors.Is(err, store.ErrStreamBusy),
		errors.Is(err, store.ErrClaimLost), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		rec.Outcome, rec.Detail = store.CellFailed, err.Error() // never the file's fault
	case err != nil:
		rec.Outcome, rec.Detail = store.CellRejected, err.Error()
	case got.Existed && got.Release.State == store.ReleaseYanked:
		rec.Outcome = store.CellYanked
	case got.Existed:
		rec.Outcome = store.CellUnchanged
	case dryRun:
		rec.Outcome = would
	default:
		rec.Outcome = store.CellReleased
	}
	if rec.Outcome != store.CellReleased && rec.Outcome != store.CellUnchanged && rec.Outcome != would && rec.Outcome != store.CellYanked {
		s.Log.Info("upstream: a bundle cell was not released", "tenant", t.Name, "upstream", u.Name, "duckdb_version", c.DuckDBVersion,
			"platform", c.Platform, "name", c.Name, "outcome", rec.Outcome, "reason", rec.Detail)
	}
	return s.record(ctx, t, tg, c, rec, dryRun)
}

// note counts a cell's outcome in its target's run.
func (tg *target) note(c bundle.Cell, outcome, detail string) {
	tg.res.Counts[outcome]++
	if outcome != store.CellReleased && outcome != store.CellUnchanged && outcome != would && len(tg.res.Problems) < maxProblems {
		tg.res.Problems = append(tg.res.Problems, CellNote{c.DuckDBVersion, c.Platform, c.Name, outcome, detail})
	}
}

// record counts a cell's outcome and, unless dry, records it as a run does, with the manifest's
// etag and fetched_at. Only a deleting tenant is returned: it stops the import.
func (s *Service) record(ctx context.Context, t store.Tenant, tg *target, c bundle.Cell, rec store.UpstreamCell, dryRun bool) error {
	k := [3]string{c.DuckDBVersion, c.Platform, c.Name}
	tg.done[k] = true
	tg.note(c, rec.Outcome, rec.Detail)
	if dryRun {
		return nil
	}
	rec.UpstreamID, rec.DuckDBVersion, rec.Platform, rec.Name = tg.u.ID, c.DuckDBVersion, c.Platform, c.Name
	rec.ETag, rec.FetchedAt = c.ETag, c.FetchedAt
	if rec.FetchedAt.IsZero() {
		rec.FetchedAt = s.Store.Now().UTC()
	}
	prev := tg.last[k]
	switch {
	case rec.Outcome == store.CellFailed && conditional[prev.Outcome]:
		return nil // never the file's fault: the record of what the cell holds stays
	case conditional[rec.Outcome]:
		if rec.BodyHash == "" {
			rec.BodyHash = c.BodyHash
		}
		if rec.BodyHash == prev.BodyHash && prev.FetchedAt.After(rec.FetchedAt) {
			rec.FetchedAt = prev.FetchedAt // the same body: its fetch time never goes back
		}
	default:
		// missing, rejected, failed: the record keeps the newest body the cell held and its fetch
		// time, the high-water mark the no-downgrade rule compares with
		rec.BodyHash, rec.FetchedAt = prev.BodyHash, cmp.Or(prev.FetchedAt, rec.FetchedAt)
	}
	wctx := context.WithoutCancel(ctx)
	err := s.Store.InTenantTx(wctx, t.ID, nil, func(tx *store.Tx) error {
		if err := tx.PutCell(wctx, rec); err != nil {
			return err
		}
		if refusal[rec.Outcome] && rec.Outcome != prev.Outcome {
			return tx.Event(wctx, t.ID, tg.res.actor, "upstream.rejected", "upstream:"+tg.u.Name,
				map[string]any{"upstream": tg.u.Name, "duckdb_version": c.DuckDBVersion, "platform": c.Platform, "name": c.Name,
					"outcome": rec.Outcome})
		}
		return nil
	})
	if errors.Is(err, store.ErrDeleting) {
		return err
	}
	if err != nil {
		s.Log.Warn("upstream: recording a bundle cell", "upstream", tg.u.Name, "error", err)
	}
	tg.last[k] = rec
	return nil
}

// leftovers records the target cells no file filled: missing, unchanged, rejected, and fetched
// cells whose file never came.
func (s *Service) leftovers(ctx context.Context, t store.Tenant, targets []*target, dryRun, ended bool) {
	for _, tg := range targets {
		if !tg.active() {
			continue
		}
		for _, c := range tg.src.Cells {
			k := [3]string{c.DuckDBVersion, c.Platform, c.Name}
			if !tg.matrix[k] || tg.done[k] {
				continue
			}
			cur, err := tg.fresh(ctx)
			if err != nil || cur.State != store.UpstreamActive {
				tg.stopped, tg.runErr = true, cmp.Or(err, errStopped)
				break
			}
			if _, ok := entry(cur, c.Name); !ok || !slices.Contains(cur.Platforms, c.Platform) {
				continue
			}
			prev := tg.last[k]
			var rec store.UpstreamCell
			switch c.Outcome {
			case bundle.Missing:
				rec = store.UpstreamCell{Outcome: store.CellMissing, Detail: bundleDetail(c.Detail)}
			case bundle.Rejected:
				rec = store.UpstreamCell{Outcome: store.CellRejected, Detail: bundleDetail(c.Detail)}
			case bundle.Unchanged:
				if conditional[prev.Outcome] && prev.BodyHash == c.BodyHash {
					tg.done[k] = true
					tg.note(c, store.CellUnchanged, "")
					// the record stays, confirmed at the source as late as the bundle says
					if !dryRun && c.FetchedAt.After(prev.FetchedAt) {
						prev.FetchedAt = c.FetchedAt
						if err := s.Store.InTenantTx(context.WithoutCancel(ctx), t.ID, nil, func(tx *store.Tx) error {
							return tx.PutCell(context.WithoutCancel(ctx), prev)
						}); errors.Is(err, store.ErrDeleting) {
							return
						} else if err != nil {
							s.Log.Warn("upstream: recording a bundle cell", "upstream", tg.u.Name, "error", err)
						}
						tg.last[k] = prev
					}
					continue
				}
				rec = store.UpstreamCell{Outcome: store.CellMissing, Detail: "bundle: not in this bundle and not held"}
			case bundle.Fetched:
				why := "bundle: the bundle ended before the file"
				if ended {
					why = "bundle: the file is not in the bundle"
				}
				rec = store.UpstreamCell{Outcome: store.CellFailed, Detail: why}
			}
			if err := s.record(ctx, t, tg, c, rec, dryRun); err != nil {
				return
			}
		}
	}
}

// bundleDetail is an exporter's reason as the site records it.
func bundleDetail(d string) string {
	if d = bundle.Text(d, 900); d == "" {
		return "bundle"
	}
	return "bundle: " + d
}

// unplanned counts the bundle's cells outside every target's matrix.
func unplanned(m *bundle.Manifest, targets []*target) int {
	n := 0
	for i := range m.Sources {
		for _, c := range m.Sources[i].Cells {
			k := [3]string{c.DuckDBVersion, c.Platform, c.Name}
			if !slices.ContainsFunc(targets, func(tg *target) bool { return tg.src == &m.Sources[i] && tg.matrix[k] }) {
				n++
			}
		}
	}
	return n
}

// finishImport records each leased target's run and the import's end.
func (s *Service) finishImport(ctx context.Context, a authz.Actor, t store.Tenant, rec *store.BundleImport, b *bundle.Bundle,
	out *ImportResult, targets []*target, fts []*ftarget, problem error) {
	fctx := context.WithoutCancel(ctx)
	ended := time.Now().UTC()
	failed := problem != nil
	stop := stopClass(problem)
	if problem != nil {
		// an entry's name, a reader's error: shown and stored as text, bounded
		out.Problem = bundle.Text(problem.Error(), 500)
		out.Stop = stop
	}
	counts := map[string]any{}
	targetCounts := map[string]map[string]int{}
	for _, tg := range targets {
		tr := TargetResult{Upstream: tg.u.Name, Status: tg.status, Detail: tg.detail}
		if tg.stopped && tg.status == TargetImported {
			tr.Status, tr.Detail = TargetFailed, "the upstream was paused or removed"
			if !errors.Is(tg.runErr, errStopped) {
				tr.Detail = tg.runErr.Error()
			}
		}
		if tg.held {
			tg.res.Ended = ended
			if tg.runErr != nil {
				tg.res.Error = tg.runErr.Error()
			}
			if problem != nil && tg.res.Error == "" {
				tg.res.Error = "the import stopped (" + stop + "): " + out.Problem
			}
			data, _ := json.Marshal(tg.res)
			class := runClass(tg.runErr)
			if tg.status == TargetStale {
				class = "stale"
			} else if class == "" && problem != nil {
				class = stop
			}
			err := s.Store.InTenantTx(fctx, t.ID, nil, func(tx *store.Tx) error {
				if err := tx.FinishUpstreamRun(fctx, tg.u.ID, string(data), false, tg.res.Started, time.Time{}); err != nil {
					return err
				}
				return tx.Event(fctx, t.ID, a.String(), "upstream.run", "upstream:"+tg.u.Name,
					map[string]any{"upstream": tg.u.Name, "dry_run": tg.res.DryRun, "counts": tg.res.Counts, "error": class,
						"bundle": tg.res.Bundle})
			})
			if err != nil {
				s.Log.Warn("upstream: recording a bundle import's run", "upstream", tg.u.Name, "error", err)
			}
		}
		if tr.Status == TargetFailed || tr.Status == TargetBusy || tr.Status == TargetStale {
			failed = true
		}
		tr.Result = tg.res
		out.Targets = append(out.Targets, tr)
		targetCounts[tg.u.Name] = tg.res.Counts
		counts[tg.u.Name] = map[string]any{"status": tr.Status, "counts": tg.res.Counts}
	}
	for _, ft := range fts {
		tr := TargetResult{Feed: ft.f.Name, Status: ft.status, Detail: ft.detail}
		if ft.held && ft.imp != nil {
			res := ft.res
			tr.FeedRun = &res
			out.Gaps += ft.imp.Gaps()
		}
		if tr.Status == TargetFailed || tr.Status == TargetBusy || tr.Status == TargetStale {
			failed = true
		}
		out.Targets = append(out.Targets, tr)
		var c map[string]int
		if tr.FeedRun != nil {
			c = tr.FeedRun.Counts
		}
		targetCounts["feed:"+ft.f.Name] = c
		counts["feed:"+ft.f.Name] = map[string]any{"status": tr.Status, "counts": c}
	}
	out.Skipped = b.Skipped()
	rec.State = store.ImportDone
	if failed {
		rec.State = store.ImportFailed
	}
	if b.Ended() {
		rec.InputSHA256 = b.SHA256()
	}
	all, _ := json.Marshal(map[string]any{"targets": counts, "unplanned": out.Unplanned, "skipped_bytes": out.Skipped,
		"problem": out.Problem, "gaps": out.Gaps})
	rec.Counts = string(all)
	// the record first and on its own: an event that fails never leaves it running
	if err := s.Store.InTenantTx(fctx, t.ID, nil, func(tx *store.Tx) error { return tx.FinishBundleImport(fctx, rec) }); err != nil {
		s.Log.Warn("upstream: recording a bundle import", "import", rec.ID, "error", err)
	}
	err := s.Store.InTenantTx(fctx, t.ID, nil, func(tx *store.Tx) error {
		return tx.Event(fctx, t.ID, a.String(), "bundle.import", "tenant:"+t.Name, map[string]any{"import": rec.ID,
			"manifest": rec.ManifestID, "created_at": rec.ManifestCreatedAt, "exporter": rec.Exporter, "bundle_key": rec.BundleKey,
			"input_sha256": rec.InputSHA256, "dry_run": rec.DryRun, "state": rec.State, "targets": targetCounts})
	})
	if err != nil {
		s.Log.Warn("upstream: recording a bundle import", "import", rec.ID, "error", err)
	}
	out.Import = *rec
}

func (s *Service) releaseLeases(ctx context.Context, targets []*target, holder string) {
	for _, tg := range targets {
		if tg.held {
			_ = s.Store.ReleaseLease(context.WithoutCancel(ctx), tg.lease, holder)
			tg.held = false
		}
	}
}

// failInterrupted rewrites a tenant's running imports whose leases expired to failed.
func (s *Service) failInterrupted(ctx context.Context, tenantID string) error {
	var ids []string
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		ids, err = tx.RunningBundleImports(ctx, tenantID)
		return err
	}); err != nil {
		return err
	}
	for _, id := range ids {
		held, err := s.Store.HeldLeases(ctx, []string{store.ImportLease(id)})
		if err != nil {
			return err
		}
		if len(held) == 0 {
			if err := s.Store.InTenantTx(ctx, tenantID, nil, func(tx *store.Tx) error { return tx.FailBundleImport(ctx, id) }); err != nil {
				return err
			}
		}
	}
	return nil
}

// Imports lists a tenant's bundle imports, the newest first; a running one whose lease expired is
// shown failed (interrupted).
func (s *Service) Imports(ctx context.Context, a authz.Actor, tenant string, limit int) ([]store.BundleImport, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	out, err := s.Store.ListBundleImports(ctx, t.ID, limit)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.interrupted(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetImport reads one of a tenant's bundle imports.
func (s *Service) GetImport(ctx context.Context, a authz.Actor, tenant, id string) (store.BundleImport, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.BundleImport{}, err
	}
	b, err := s.Store.GetBundleImport(ctx, t.ID, id)
	if err != nil {
		return b, err
	}
	return b, s.interrupted(ctx, &b)
}

func (s *Service) interrupted(ctx context.Context, b *store.BundleImport) error {
	if b.State != store.ImportRunning {
		return nil
	}
	held, err := s.Store.HeldLeases(ctx, []string{store.ImportLease(b.ID)})
	if err == nil && len(held) == 0 {
		b.State, b.Interrupted = store.ImportFailed, true
	}
	return err
}

// ResetBundle clears a bundle-mode upstream's freshness mark (spec 0014), so that it takes bundles
// older than the last one it imported: the recovery from a bundle whose created_at was wrong. It is
// a server administrator's act.
func (s *Service) ResetBundle(ctx context.Context, a authz.Actor, tenant, name string) (store.Upstream, error) {
	t, err := s.serverTenant(ctx, a, tenant)
	if err != nil {
		return store.Upstream{}, err
	}
	var u store.Upstream
	err = s.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		if u, err = tx.GetUpstream(ctx, t.ID, name); err != nil {
			return err
		}
		if u.Mode != store.ModeBundle {
			return fmt.Errorf("%w: upstream %s is not in bundle mode", store.ErrInvalid, name)
		}
		if err := tx.SetBundleMark(ctx, u.ID, "", time.Time{}, ""); err != nil {
			return err
		}
		data := map[string]any{"upstream": u.Name, "manifest": u.BundleID}
		if !u.BundleCreatedAt.IsZero() {
			data["created_at"] = u.BundleCreatedAt
		}
		return tx.Event(ctx, t.ID, a.String(), "bundle.reset", "upstream:"+u.Name, data)
	})
	return u, err
}

// watched is an import's input, read through a pump so that a stall ends the import even when
// the read itself never returns (standard input takes no deadline): the watchdog closes the
// pipe's writing end with the stall, which the import then reads.
type watched struct {
	pr     *io.PipeReader
	pw     *io.PipeWriter
	mu     sync.Mutex
	since  time.Time // the read waiting now; zero: none
	waited time.Duration
	n      int64
	quit   chan struct{}
	once   sync.Once
}

func watch(ctx context.Context, r io.Reader, idle, floor time.Duration, rate int64) *watched {
	pr, pw := io.Pipe()
	w := &watched{pr: pr, pw: pw, quit: make(chan struct{})}
	go func() { // the pump: blocked in r.Read for as long as the input says nothing
		buf := make([]byte, 256<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if _, werr := pw.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	go func() { // the watchdog counts only the time the import waits for its input
		tick := min(idle/4, time.Second)
		tk := time.NewTicker(tick)
		defer tk.Stop()
		for {
			select {
			case <-w.quit:
				return
			case <-ctx.Done():
				pw.CloseWithError(context.Cause(ctx))
				return
			case <-tk.C:
			}
			w.mu.Lock()
			waited, n := w.waited, w.n
			var now time.Duration
			if !w.since.IsZero() {
				now = time.Since(w.since)
				waited += now
			}
			w.mu.Unlock()
			switch {
			case now > idle:
				pw.CloseWithError(fmt.Errorf("%w: nothing for %s", errStall, idle))
				return
			case waited > floor && float64(n)/waited.Seconds() < float64(rate):
				pw.CloseWithError(fmt.Errorf("%w: under %d bytes per second", errStall, rate))
				return
			}
		}
	}()
	return w
}

func (w *watched) Read(p []byte) (int, error) {
	w.mu.Lock()
	w.since = time.Now()
	w.mu.Unlock()
	n, err := w.pr.Read(p)
	w.mu.Lock()
	w.waited += time.Since(w.since)
	w.since = time.Time{}
	w.n += int64(n)
	w.mu.Unlock()
	return n, err
}

// stop ends the watchdog and lets the pump go: its read may stay blocked until the process exits.
func (w *watched) stop() {
	w.once.Do(func() {
		close(w.quit)
		w.pw.CloseWithError(errors.New("upstream: the import ended"))
		w.pr.CloseWithError(errors.New("upstream: the import ended")) // a pump still writing returns
	})
}
