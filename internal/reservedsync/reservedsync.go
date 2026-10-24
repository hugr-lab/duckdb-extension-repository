// Package reservedsync follows duckdb/community-extensions (spec 0030 part 3): every interval one
// replica reads the names of the repository's extensions/*/description.yml and reserves the new ones
// as community, with the source community-sync@<commit>. A failed read is logged and retried at the
// next interval; it never removes a name.
package reservedsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/duckdbsrc"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// lease spaces the passes across replicas: it is taken for most of an interval and not released.
const lease = "kista/reserved/community-sync"

// Actor is who the sync's changes are recorded as.
const Actor = "system:community-sync"

// MaxNew bounds the names one pass reserves: duckdb/community-extensions grows by a few a week, so
// more is a changed or tampered source, left to an administrator.
const MaxNew = 100

// Sync reserves duckdb/community-extensions' names.
type Sync struct {
	Store  *store.Store
	Fetch  *egress.Client
	URL    string        // a gzip tarball of the repository's default branch
	Every  time.Duration // the interval
	Holder string
	Log    *slog.Logger
}

// Run makes a pass every interval until ctx ends, the first at once: a pass runs when this replica
// takes the lease, which spans most of an interval (after a restart, or a shorter interval
// configured, the lease taken before still runs out first).
func (s *Sync) Run(ctx context.Context) {
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		if added, err := s.Once(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("reserved: the community sync failed; it runs again at the next interval", "url", s.URL, "error", err)
			s.failed(ctx, err)
		} else if len(added) > 0 {
			s.Log.Info("reserved: community names reserved", "count", len(added))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Errors a pass may end with, as its failed event names them (never the error's text).
var (
	errFetch   = errors.New("reservedsync: the tarball could not be read")
	errTooMany = errors.New("reservedsync: too many new names")
)

// failed records a failed pass (spec 0010: a background job's failure is an event, with a class).
func (s *Sync) failed(ctx context.Context, err error) {
	class := "error"
	switch {
	case errors.Is(err, errFetch):
		class = "fetch"
	case errors.Is(err, errTooMany):
		class = "too_many"
	}
	e, eerr := s.Store.NewEvent(ctx, audit.ServerTenant, Actor, "reserved.sync", "failed", "sync:community", map[string]any{"error": class})
	if eerr == nil {
		eerr = s.Store.InsertEvents(ctx, []store.Event{e})
	}
	if eerr != nil {
		s.Log.Error("reserved: recording the community sync's failure", "error", eerr)
	}
}

// Once makes one pass if it takes the lease, and returns the names it reserved.
func (s *Sync) Once(ctx context.Context) ([]string, error) {
	leased, err := s.Store.AcquireLease(ctx, lease, s.Holder, s.Every*9/10)
	if err != nil || !leased {
		if err == nil {
			s.Log.Debug("reserved: the community sync ran on another replica (or before a restart) this interval")
		}
		return nil, err
	}
	names, commit, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	kinds, _, err := s.Store.ReservedNames(ctx)
	if err != nil {
		return nil, err
	}
	// a name a tenant publishes under is left to an administrator (kista admin reserved add -force):
	// reserving it would narrow that tenant's wide grants on a third party's say
	var fresh []string
	held := map[string][]string{}
	for _, n := range names {
		if kinds[pkgtype.Of(n)] != "" { // DuckDB's community names are duckdb_extension's (spec 0034)
			continue
		}
		ts, err := s.Store.TenantsWithReleases(ctx, pkgtype.DuckDBExtension, n)
		if err != nil {
			return nil, err
		}
		if len(ts) > 0 {
			held[n] = ts
			s.Log.Warn("reserved: a community name tenants publish under is not reserved; an administrator decides",
				"name", n, "tenants", ts)
			continue
		}
		fresh = append(fresh, n)
	}
	if len(fresh) > MaxNew {
		return nil, fmt.Errorf("%w: %d in one pass, more than %d; a source that changed this much is left to an administrator "+
			"(kista duckdb community-extensions, kista admin reserved add)", errTooMany, len(fresh), MaxNew)
	}
	s.Log.Info("reserved: community sync read", "commit", commit, "new", len(fresh), "skipped", len(held))
	if len(fresh) == 0 {
		return nil, nil
	}
	src := "community-sync@" + commit
	var added []string
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		added = nil
		for _, n := range fresh {
			// reserved since it was read (another writer): left as it is, without this source
			if k, err := tx.ReservedKind(ctx, pkgtype.DuckDBExtension, n); err != nil || k != "" {
				if err != nil {
					return err
				}
				continue
			}
			changed, err := tx.ReserveName(ctx, pkgtype.DuckDBExtension, n, store.ReservedCommunity, src)
			if err != nil {
				return err
			}
			if changed {
				added = append(added, n)
			}
		}
		if len(added) == 0 {
			return nil
		}
		if err := tx.BumpReservedVersion(ctx); err != nil {
			return err
		}
		f := map[string]any{"kind": store.ReservedCommunity, "names": added, "source": src}
		if len(held) > 0 {
			f["skipped"] = held
		}
		return tx.Event(ctx, "", Actor, "reserved.add", "sync:community", f)
	})
	return added, err
}

// Bounds of a read: the tarball (the repository is a few MB) and its pace.
const (
	maxTarball = 256 << 20
	timeout    = 10 * time.Minute
	minRate    = 16 << 10
)

// read streams the tarball through egress and parses its names; the commit is the one git archive
// wrote, required (a source names it).
func (s *Sync) read(ctx context.Context) ([]string, string, error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := s.Fetch.Download(ctx, s.URL, egress.DownloadOptions{MaxBytes: maxTarball, Timeout: timeout, MinRate: minRate}, pw)
		pw.CloseWithError(err)
		done <- err
	}()
	src, commit, err := duckdbsrc.Read(pr, duckdbsrc.CommunityFiles)
	if err == nil {
		_, _ = io.Copy(io.Discard, pr) // the stream's tail (padding after the archive's end)
	}
	pr.CloseWithError(errors.New("reservedsync: read ended")) // a download still writing stops
	if derr := <-done; derr != nil && err == nil {
		err = derr
	}
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s: %w", errFetch, s.URL, err)
	}
	if commit == "" {
		return nil, "", fmt.Errorf("%w: %s names no commit (a tarball git archive wrote does)", errFetch, s.URL)
	}
	names, err := duckdbsrc.CommunityNames(src)
	if err != nil {
		return nil, "", err
	}
	return slices.Compact(names), commit, nil
}
