package release_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// counting is a store that counts the icons put and read.
type counting struct {
	blob.Store
	puts, gets atomic.Int32
}

func (c *counting) Put(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	if strings.HasPrefix(key, "icons/") {
		c.puts.Add(1)
	}
	return c.Store.Put(ctx, key, r, size)
}

func (c *counting) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "icons/") {
		c.gets.Add(1)
	}
	return c.Store.Get(ctx, key, off, n)
}

// counted is the env's release service over a store that counts its icons.
func counted(t *testing.T, en *env) (*release.Service, *counting) {
	t.Helper()
	c := &counting{Store: en.blobs}
	bs, err := blob.NewService(ctx, en.st, []blob.Domain{{Name: "default", Kind: "fs", Store: c}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 64 << 20, MaxIngests: 4, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bs.Close() })
	rel := *en.rel
	rel.Blob = bs
	return &rel, c
}

// Spec 0036 (review): a token's write never moves the mark back, and a token's no-op writes nothing.
func TestEntryMetaTokenMark(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		_, _, distPub := metaEnv(t, en)
		rel := en.rel
		now := time.Now().UTC().Truncate(time.Second)
		en.st.Now = func() time.Time { return now }
		signed := func(at time.Time, b, summary string) error {
			_, err := rel.SetEntryMeta(ctx, distPub, "acme", "", "publisher:enterest-ci", "geo",
				release.MetaWrite{Summary: str(summary), SignedAt: at, BodySHA256: strings.Repeat(b, 64)})
			return err
		}
		must(t, func() error { return signed(now.Add(4*time.Minute), "a", "ahead") }) // a signed-at ahead of the server's clock
		if _, err := rel.SetEntryMeta(ctx, distAdmin, "acme", "", "publisher:enterest-ci", "geo", release.MetaWrite{Summary: str("token")}); err != nil {
			t.Fatal(err)
		}
		m, _ := rel.EntryMeta(ctx, admin, "acme", "", "publisher:enterest-ci", "geo")
		if !m.SignedAt.Equal(now.Add(4*time.Minute)) || m.SignedBody != "" || m.Summary != "token" {
			t.Fatalf("the mark after a token's write: %+v", m)
		}
		if err := signed(now.Add(2*time.Minute), "b", "between"); !errors.Is(err, release.ErrStale) {
			t.Fatalf("a request signed before the mark: %v", err)
		}
		// a token's no-op: nothing written, the mark and who set it kept
		now = now.Add(10 * time.Minute)
		if _, err := rel.SetEntryMeta(ctx, admin, "acme", "", "publisher:enterest-ci", "geo", release.MetaWrite{Summary: str("token")}); err != nil {
			t.Fatal(err)
		}
		if m2, _ := rel.EntryMeta(ctx, admin, "acme", "", "publisher:enterest-ci", "geo"); m2.EntryMeta != m.EntryMeta {
			t.Fatalf("a token's no-op wrote: %+v, was %+v", m2.EntryMeta, m.EntryMeta)
		}
		// a token's no-op on an entry without metadata writes no row
		if _, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{}); err != nil {
			t.Fatal(err)
		}
		tn, _ := en.st.GetTenant(ctx, "acme")
		if _, ok, _ := en.st.EntryMeta(ctx, tn.ID, "duckdb_extension", "tenant", "acl"); ok {
			t.Fatal("a token's no-op wrote a row")
		}
	})
}

// Spec 0036 (review): a write is checked before anything is uploaded (stale, a kept icon changed,
// the byte cap's growth); the icon the entry has is not uploaded again; a refused reader reads
// nothing from the store.
func TestEntryMetaChecksBeforeUpload(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		_, _, distPub := metaEnv(t, en)
		rel, c := counted(t, en)
		ic := newIcon(t, 64)
		at := time.Now().UTC().Truncate(time.Second)
		write := func(a authz.Actor, at time.Time, b string, icon *release.IconInput) error {
			w := release.MetaWrite{Icon: icon}
			if !at.IsZero() {
				w.SignedAt, w.BodySHA256 = at, strings.Repeat(b, 64)
			}
			_, err := rel.SetEntryMeta(ctx, a, "acme", "", "publisher:enterest-ci", "geo", w)
			return err
		}
		must(t, func() error { return write(distPub, at, "a", ic) })
		if c.puts.Load() != 1 {
			t.Fatalf("puts: %d", c.puts.Load())
		}
		if err := write(distPub, at.Add(-time.Second), "b", newIcon(t, 96)); !errors.Is(err, release.ErrStale) || c.puts.Load() != 1 {
			t.Fatalf("a stale write uploaded: %v, %d puts", err, c.puts.Load())
		}
		must(t, func() error { return write(distPub, at.Add(time.Second), "c", ic) }) // the same icon again: not uploaded
		if c.puts.Load() != 1 {
			t.Fatalf("the same icon uploaded again: %d puts", c.puts.Load())
		}
		// the cap counts the growth only: a smaller icon passes a cap the source is at
		tn, _ := en.st.GetTenant(ctx, "acme")
		src, _ := en.st.ResolveSource(ctx, tn.ID, "publisher:enterest-ci")
		_, used, _ := en.st.SourceUsage(ctx, tn.ID, src)
		rel.MaxBytes = used / 2 // over the cap already
		if err := write(distPub, at.Add(2*time.Second), "d", newIcon(t, 128)); !errors.Is(err, store.ErrCatalogueFull) || c.puts.Load() != 1 {
			t.Fatalf("a larger icon over the cap: %v, %d puts", err, c.puts.Load())
		}
		var plain bytes.Buffer // a uniform icon: smaller than the one stored
		if err := png.Encode(&plain, image.NewRGBA(image.Rect(0, 0, 64, 64))); err != nil || int64(plain.Len()) >= int64(len(ic.Data)) {
			t.Fatalf("the smaller icon: %d bytes, %v", plain.Len(), err)
		}
		if err := write(distPub, at.Add(3*time.Second), "e", &release.IconInput{ContentType: "image/png", Data: plain.Bytes()}); err != nil {
			t.Fatalf("a smaller icon over the cap: %v", err)
		}
		rel.MaxBytes = 0
		// a refused reader reads nothing; an allowed one reads the object once (the same-icon write above
		// read it once, to check it is there)
		c.gets.Store(0)
		if _, err := rel.EntryIcon(ctx, tenantAdmin, "acme", "", "publisher:ci", "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("no entry: %v", err)
		}
		if _, err := rel.EntryIcon(ctx, distAdmin, "acme", "", "publisher:ci", "tile"); err == nil {
			t.Fatal("a distribution admin read the tenant's entry")
		}
		if c.gets.Load() != 0 {
			t.Fatalf("a refused read reached the store: %d gets", c.gets.Load())
		}
		if _, err := rel.EntryIcon(ctx, tenantAdmin, "acme", "", "publisher:enterest-ci", "geo"); err != nil || c.gets.Load() != 1 {
			t.Fatalf("an allowed read: %v, %d gets", err, c.gets.Load())
		}
	})
}

// Spec 0036 (review): the write's transaction checks again, under the tenant's auth lock, what
// changed since its checks: a name reserved server-wide, a tenant suspended, a key revoked.
// An icon uploaded for a write that fails is released to the collector. A claim lost is claimed and
// uploaded again.
func TestEntryMetaChecksAgain(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		_, dist, distPub := metaEnv(t, en)
		rel, c := counted(t, en)
		tn, _ := en.st.GetTenant(ctx, "acme")
		n := 0
		signed := func() release.MetaWrite {
			n++
			return release.MetaWrite{Icon: newIcon(t, 64+n), SignedAt: time.Now().UTC().Truncate(time.Second).Add(time.Duration(n) * time.Second),
				BodySHA256: strings.Repeat("a", 64)}
		}
		// released: no live claim holds the icon, so the collector may mark it
		released := func(w release.MetaWrite) bool {
			ok, err := en.st.Icons().Mark(ctx, "default", blob.ContentHash(w.Icon.Data))
			if err != nil {
				t.Fatal(err)
			}
			return ok
		}
		// a name reserved server-wide meanwhile
		t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
		release.SetBeforeMetaCommit(rel, func() { reserveServerWide(t, en.st, "geo") })
		onReserved := signed()
		_, err := rel.SetEntryMeta(ctx, distPub, "acme", "", "publisher:enterest-ci", "geo", onReserved)
		var up *release.UploadedError
		if !errors.Is(err, authz.ErrDenied) || !errors.As(err, &up) || c.puts.Load() != 1 || !released(onReserved) {
			t.Fatalf("a name reserved meanwhile: %v, %d puts", err, c.puts.Load())
		}
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				_, err := tx.RemoveReservedName(ctx, "duckdb_extension", "geo")
				return err
			})
		})
		// a key revoked meanwhile
		k := store.PublisherKey{TenantID: tn.ID, PublisherID: dist.ID, Type: "ssh-ed25519", PublicKey: "ssh-ed25519 AAAA",
			Fingerprint: "SHA256:" + strings.Repeat("k", 43), CreatedBy: "os:1:t"}
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &k) })
		})
		release.SetBeforeMetaCommit(rel, func() {
			must(t, func() error {
				return en.st.InTx(ctx, "", func(tx *store.Tx) error { _, err := tx.DeletePublisherKey(ctx, dist.ID, k.ID); return err })
			})
		})
		w := signed()
		w.KeyID = k.ID
		if _, err := rel.SetEntryMeta(ctx, distPub, "acme", "", "publisher:enterest-ci", "geo", w); !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("a key revoked meanwhile: %v", err)
		}
		// the tenant suspended meanwhile: a distribution admin's write is refused
		release.SetBeforeMetaCommit(rel, func() {
			must(t, func() error {
				return en.st.InTx(ctx, "", func(tx *store.Tx) error {
					cur, err := tx.GetTenantByID(ctx, tn.ID)
					if err != nil {
						return err
					}
					return tx.SetTenantState(ctx, &cur, store.TenantSuspended)
				})
			})
		})
		if _, err := rel.SetEntryMeta(ctx, distAdmin, "acme", "", "publisher:enterest-ci", "geo", release.MetaWrite{Summary: str("x")}); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("a tenant suspended meanwhile: %v", err)
		}
		release.SetBeforeMetaCommit(rel, nil)
		if _, err := rel.SetEntryMeta(ctx, distPub, "acme", "", "publisher:enterest-ci", "geo", signed()); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("a publisher on a suspended tenant: %v", err)
		}
	})
}

// Spec 0036 (review): a claim lost before the row's transaction is claimed and uploaded again.
func TestEntryMetaClaimLost(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		metaEnv(t, en)
		rel, c := counted(t, en)
		var shift atomic.Int64
		en.st.Now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
		first := true
		release.SetBeforeMetaCommit(rel, func() {
			if first {
				first = false
				shift.Add(int64(2 * time.Hour)) // the first claim expires
			}
		})
		v, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{Icon: newIcon(t, 64)})
		if err != nil || v.IconSHA256 == "" || c.puts.Load() != 2 {
			t.Fatalf("a claim lost: %+v %v, %d puts", v, err, c.puts.Load())
		}
	})
}
