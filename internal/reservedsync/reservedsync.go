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

	"github.com/hugr-lab/duckdb-extension-repository/internal/duckdbsrc"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// lease spaces the passes across replicas: it is taken for most of an interval and not released.
const lease = "kista/reserved/community-sync"

// Actor is who the sync's changes are recorded as.
const Actor = "system:community-sync"

// Sync reserves duckdb/community-extensions' names.
type Sync struct {
	Store  *store.Store
	Fetch  *egress.Client
	URL    string        // a gzip tarball of the repository's default branch
	Every  time.Duration // the interval
	Holder string
	Log    *slog.Logger
}

// Run makes a pass now and every interval until ctx ends.
func (s *Sync) Run(ctx context.Context) {
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		if added, err := s.Once(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("reserved: the community sync failed; it runs again at the next interval", "url", s.URL, "error", err)
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

// Once makes one pass if it takes the lease, and returns the names it reserved.
func (s *Sync) Once(ctx context.Context) ([]string, error) {
	held, err := s.Store.AcquireLease(ctx, lease, s.Holder, s.Every*9/10)
	if err != nil || !held {
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
	var fresh []string
	for _, n := range names {
		if kinds[n] == "" {
			fresh = append(fresh, n)
		}
	}
	if len(fresh) == 0 {
		return nil, nil
	}
	src := "community-sync@" + commit
	var added []string
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		added = nil
		for _, n := range fresh {
			changed, err := tx.ReserveName(ctx, n, store.ReservedCommunity, src)
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
		return tx.Event(ctx, "", Actor, "reserved.add", "sync:community", map[string]any{"kind": store.ReservedCommunity,
			"names": added, "source": src})
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
		return nil, "", fmt.Errorf("reading %s: %w", s.URL, err)
	}
	if commit == "" {
		return nil, "", fmt.Errorf("%s names no commit (a tarball git archive wrote does)", s.URL)
	}
	names, err := duckdbsrc.CommunityNames(src)
	if err != nil {
		return nil, "", err
	}
	return slices.Compact(names), commit, nil
}
