package upstream

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// The import of statements (spec 0014 phase 1b): a bundle's feeds into the tenant's bundle-mode
// feeds, each under its lease (shared with online runs), after the same freshness checks as an
// upstream target, read after the files.

// ftarget is a bundle-mode feed an import fills.
type ftarget struct {
	f      store.Feed
	fi     int // the manifest's feed
	lease  string
	held   bool
	status string
	detail string
	imp    *attachment.FeedImport
	res    attachment.RunResult
}

// feedTargets selects the tenant's bundle-mode feeds the import fills: the named ones (each must
// be in bundle mode, active, and have a feed in the bundle), or every active one whose URL is a
// feed of the bundle.
func (s *Service) feedTargets(ctx context.Context, t store.Tenant, m *bundle.Manifest, names []string) ([]*ftarget, error) {
	if s.Feeds == nil {
		if len(names) > 0 {
			return nil, fmt.Errorf("%w: this server imports no feed", store.ErrInvalid)
		}
		return nil, nil
	}
	var all []store.Feed
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		all, err = tx.ListFeeds(ctx, t.ID)
		return err
	}); err != nil {
		return nil, err
	}
	feed := func(f store.Feed) int {
		return slices.IndexFunc(m.Feeds, func(mf bundle.Feed) bool { return mf.URL == f.URL })
	}
	for _, n := range names {
		i := slices.IndexFunc(all, func(f store.Feed) bool { return f.Name == n })
		switch {
		case i < 0:
			return nil, fmt.Errorf("%w: feed %s", store.ErrNotFound, n)
		case all[i].Mode != store.FeedBundle:
			return nil, fmt.Errorf("%w: feed %s is not in bundle mode", store.ErrInvalid, n)
		case all[i].State != store.FeedActive:
			return nil, fmt.Errorf("%w: feed %s is paused", store.ErrInvalid, n)
		case feed(all[i]) < 0:
			return nil, fmt.Errorf("%w: the bundle holds no feed of %s", store.ErrInvalid, n)
		}
	}
	var out []*ftarget
	for _, f := range all {
		if f.Mode != store.FeedBundle || f.State != store.FeedActive || len(names) > 0 && !slices.Contains(names, f.Name) {
			continue
		}
		if i := feed(f); i >= 0 {
			out = append(out, &ftarget{f: f, fi: i, lease: "kista/feed/" + f.ID, status: TargetImported})
		}
	}
	return out, nil
}

// prepareFeed checks a leased feed target's freshness against the bundle and records the mark
// unless dry (before its first statement, as an upstream's), then starts its import.
func (s *Service) prepareFeed(ctx context.Context, t store.Tenant, ft *ftarget, m *bundle.Manifest, manifestSHA string, dryRun bool) {
	created := m.CreatedAt.Truncate(time.Microsecond)
	stale := ""
	err := s.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		f, err := tx.GetFeedByID(ctx, ft.f.ID)
		if err != nil {
			return err
		}
		switch {
		case f.State != store.FeedActive || f.Mode != store.FeedBundle:
			return fmt.Errorf("%w: the feed was paused or changed", errStopped)
		case m.Plan.CreatedAt.Before(f.CreatedAt):
			stale = "the bundle's plan is older than the feed"
		case f.BundleID == m.ID && f.BundleManifestSHA256 != manifestSHA:
			stale = "a bundle of the same id with another manifest was imported"
		case f.BundleID != "" && f.BundleID != m.ID && !after(created, m.ID, f.BundleCreatedAt.Truncate(time.Microsecond), f.BundleID):
			stale = "a newer bundle was imported (" + f.BundleID + ")"
		}
		ft.f = f
		if stale != "" || dryRun {
			return nil
		}
		return tx.SetFeedBundleMark(ctx, f.ID, m.ID, created, manifestSHA)
	})
	switch {
	case err != nil:
		ft.status, ft.detail = TargetFailed, err.Error()
		return
	case stale != "":
		ft.status, ft.detail = TargetStale, "upstream.stale: "+stale
		return
	}
	mf := m.Feeds[ft.fi]
	ranges := map[string][2]int64{}
	for _, b := range mf.Bodies {
		ranges[b.BodyHash] = [2]int64{b.AfterSeq, b.Seq}
	}
	var brought []string
	for _, src := range m.Sources {
		for _, c := range src.Cells {
			if c.Outcome == bundle.Fetched {
				brought = append(brought, c.BodyHash)
			}
		}
	}
	ft.imp, err = s.Feeds.ImportFeed(ctx, t, ft.f, m.ID, ranges, brought, dryRun, func(context.Context) error { return nil })
	if err != nil {
		ft.status, ft.detail = TargetFailed, err.Error()
	}
}

func (ft *ftarget) active() bool { return ft.status == TargetImported && ft.imp != nil }

// feedEntry hands a feed's entry to its targets: the keys file, a batch, a document; a damaged
// entry fails them.
func (s *Service) feedEntry(ctx context.Context, it bundle.Item, fts []*ftarget) error {
	fe, _ := bundle.ParseFeedEntry(it.Feed.Entries[it.Entry].Path)
	var batch []attachment.BundleStatement
	if it.Damaged == "" && fe.Batch > 0 {
		entries, err := bundle.ParseBatch(it.Data)
		if err != nil {
			it.Damaged = "a batch does not parse: " + err.Error()
		}
		for _, e := range entries {
			st, err1 := base64.StdEncoding.DecodeString(e.Statement)
			sig, err2 := base64.StdEncoding.DecodeString(e.Signature)
			if err1 != nil || err2 != nil {
				continue // ParseBatch checked the encoding
			}
			batch = append(batch, attachment.BundleStatement{Statement: st, Signature: sig, Note: e.Note})
		}
	}
	for _, ft := range fts {
		if ft.fi != it.FeedIndex || !ft.active() {
			continue
		}
		switch {
		case it.Damaged != "":
			ft.imp.Fail(fmt.Errorf("%w: %s", bundle.ErrMalformed, it.Damaged))
		case fe.Keys:
			ft.imp.Keys(it.Data)
		case fe.Batch > 0:
			if err := ft.imp.Batch(ctx, batch); err != nil {
				return err
			}
		default:
			if err := ft.imp.Document(fe.Document, it.Data); err != nil {
				ft.imp.Fail(err)
			}
		}
	}
	return nil
}

// finishFeeds ends each feed target: its last batch, its cursors when its entries were read whole,
// its run's record.
func (s *Service) finishFeeds(ctx context.Context, a authz.Actor, b *bundle.Bundle, fts []*ftarget, stopped bool) {
	for _, ft := range fts {
		if !ft.active() {
			continue
		}
		res, err := ft.imp.Finish(ctx, a.String(), !stopped && b.FeedWhole(ft.fi))
		ft.res = res
		if ferr := ft.imp.Failed(); ferr != nil {
			ft.status, ft.detail = TargetFailed, ferr.Error()
		}
		if err != nil {
			s.Log.Warn("upstream: recording a bundle import's feed run", "feed", ft.f.Name, "error", err)
		}
	}
}

// ResetFeedBundle clears a bundle-mode feed's last-bundle mark and its cursors (spec 0014 phase
// 1b), so a later bundle takes its history again (statements held are no-ops): the recovery after
// a bundle that left statements out or lacked their content.
func (s *Service) ResetFeedBundle(ctx context.Context, a authz.Actor, tenant, name string) (store.Feed, error) {
	t, err := s.serverTenant(ctx, a, tenant)
	if err != nil {
		return store.Feed{}, err
	}
	var f store.Feed
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.FeedLock(t.ID)}, func(tx *store.Tx) error {
		if f, err = tx.GetFeed(ctx, t.ID, name); err != nil {
			return err
		}
		if f.Mode != store.FeedBundle {
			return fmt.Errorf("%w: feed %s is not in bundle mode", store.ErrInvalid, name)
		}
		if err := tx.SetFeedBundleMark(ctx, f.ID, "", time.Time{}, ""); err != nil {
			return err
		}
		if err := tx.ResetFeedCursors(ctx, f.ID); err != nil {
			return err
		}
		data := map[string]any{"feed": f.Name, "manifest": f.BundleID}
		if !f.BundleCreatedAt.IsZero() {
			data["created_at"] = f.BundleCreatedAt
		}
		return tx.Event(ctx, t.ID, a.String(), "bundle.reset", "feed:"+f.Name, data)
	})
	return f, err
}
