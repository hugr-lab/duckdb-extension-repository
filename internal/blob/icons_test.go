package blob_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// iconTenant is a tenant of the default domain.
func iconTenant(t *testing.T, e *env) store.Tenant {
	t.Helper()
	tn := store.Tenant{Name: "acme", DisplayName: "Acme"}
	if err := e.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, &tn) }); err != nil {
		t.Fatal(err)
	}
	return tn
}

// writeIcon writes an entry's icon as the service does: claimed and uploaded, then the row with its
// claim committed.
func writeIcon(t *testing.T, e *env, svc *blob.Service, tn store.Tenant, name string, data []byte) string {
	t.Helper()
	sha, claim, err := svc.PutIcon(ctx, "default", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.InTxLocks(ctx, []string{store.PublisherAuthLock(tn.ID), store.IconLock("default", sha)}, func(tx *store.Tx) error {
		if err := tx.CommitIcon(ctx, "default", sha, claim); err != nil {
			return err
		}
		return tx.PutEntryMeta(ctx, store.EntryMeta{TenantID: tn.ID, PackageType: "duckdb_extension", Source: "tenant", Name: name,
			IconSHA256: sha, IconWidth: 64, IconHeight: 64, IconBytes: int64(len(data)), SetBy: "os:1:t", SetAt: e.st.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	return sha
}

// Spec 0036: the collector marks an icon no entry's metadata references and no live claim holds,
// and deletes it a grace later; a referenced or claimed icon stays; a read is checked against the row.
func TestIconCollector(t *testing.T) {
	e := newEnv(t, blob.Options{})
	now := time.Now().UTC()
	e.st.Now = func() time.Time { return now }
	tn := iconTenant(t, e)
	used := writeIcon(t, e, e.svc, tn, "acl", []byte("used icon"))
	// a write that gave up, and one in flight (claimed, not committed)
	gaveUp, claim, err := e.svc.PutIcon(ctx, "default", []byte("given up"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ReleaseIcon(ctx, "default", gaveUp, claim); err != nil {
		t.Fatal(err)
	}
	inFlight, _, err := e.svc.PutIcon(ctx, "default", []byte("in flight"))
	if err != nil {
		t.Fatal(err)
	}
	exists := func(sha string) bool {
		_, err := e.fsStore.Stat(ctx, blob.IconKey(sha))
		return err == nil
	}
	c := &blob.Collector{Service: e.svc, Holder: "test", Grace: 24 * time.Hour, Log: slog.New(slog.DiscardHandler)}
	if r, err := c.Pass(ctx, "default", false); err != nil || r.Marked != 1 || r.Icons != 0 {
		t.Fatalf("the first pass marks the given-up icon only: %+v %v", r, err)
	}
	// a grace later: the given-up icon goes; the claim in flight expired, so its icon is marked now
	now = now.Add(25 * time.Hour)
	if r, err := c.Pass(ctx, "default", false); err != nil || r.Icons != 1 || r.Marked != 1 || r.Claims != 1 {
		t.Fatalf("the second pass: %+v %v", r, err)
	}
	if exists(gaveUp) || !exists(inFlight) || !exists(used) {
		t.Fatalf("after the second pass: given up %v, in flight %v, used %v", exists(gaveUp), exists(inFlight), exists(used))
	}
	// the metadata cleared: the icon is unreferenced, marked, and deleted a grace later
	if err := e.st.InTx(ctx, store.PublisherAuthLock(tn.ID), func(tx *store.Tx) error {
		return tx.PutEntryMeta(ctx, store.EntryMeta{TenantID: tn.ID, PackageType: "duckdb_extension", Source: "tenant", Name: "acl",
			SetBy: "os:1:t", SetAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(25 * time.Hour)
	if r, err := c.Pass(ctx, "default", false); err != nil || r.Icons != 1 || r.Marked != 1 {
		t.Fatalf("the third pass: %+v %v", r, err)
	}
	now = now.Add(25 * time.Hour)
	if r, err := c.Pass(ctx, "default", false); err != nil || r.Icons != 1 {
		t.Fatalf("the fourth pass: %+v %v", r, err)
	}
	if exists(used) || exists(inFlight) {
		t.Fatal("an unreferenced icon survived")
	}
}

// A write meeting an icon the collector is deleting waits, then uploads it: the object is there
// when the row is written. A delete of unknown outcome is finished by the resolver.
func TestIconWriteWaitsForADelete(t *testing.T) {
	blob.SetBusyPoll(t, 10*time.Millisecond)
	e := newEnv(t, blob.Options{})
	var clock atomic.Int64 // the write reads it while the test moves it
	clock.Store(time.Now().UnixNano())
	e.st.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	later := func(d time.Duration) { clock.Add(int64(d)) }
	tn := iconTenant(t, e)
	data := []byte("an icon")
	sha, claim, err := e.svc.PutIcon(ctx, "default", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ReleaseIcon(ctx, "default", sha, claim); err != nil {
		t.Fatal(err)
	}
	// the collector's delete times out: the tombstone stays deleting
	f := &faulty{delete: func(string) error { return context.DeadlineExceeded }}
	c, _ := collector(t, e, f)
	if _, err := c.Pass(ctx, "default", false); err != nil {
		t.Fatal(err)
	}
	later(25 * time.Hour)
	if _, err := c.Pass(ctx, "default", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a delete of unknown outcome: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- writeIconErr(e, tn, data) }()
	select {
	case err := <-done:
		t.Fatalf("a write went on during the delete: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// the resolver, once the delete is stale, finishes it; the write goes on and uploads again
	f.delete = nil
	later(40 * time.Minute)
	if r, err := c.Resolve(ctx, "default"); err != nil || r.Resolved != 1 || r.Icons != 1 {
		t.Fatalf("the resolver: %+v %v", r, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write never went on")
	}
	got, err := e.svc.ReadIcon(ctx, "default", sha, int64(len(data)))
	if err != nil || string(got) != string(data) {
		t.Fatalf("the icon after the write: %q %v", got, err)
	}
}

func writeIconErr(e *env, tn store.Tenant, data []byte) error {
	sha, claim, err := e.svc.PutIcon(ctx, "default", data)
	if err != nil {
		return err
	}
	return e.st.InTxLocks(ctx, []string{store.PublisherAuthLock(tn.ID), store.IconLock("default", sha)}, func(tx *store.Tx) error {
		if err := tx.CommitIcon(ctx, "default", sha, claim); err != nil {
			return err
		}
		return tx.PutEntryMeta(ctx, store.EntryMeta{TenantID: tn.ID, PackageType: "duckdb_extension", Source: "tenant", Name: "acl",
			IconSHA256: sha, IconWidth: 64, IconHeight: 64, IconBytes: int64(len(data)), SetBy: "os:1:t", SetAt: e.st.Now()})
	})
}

// A read is checked against the row: a missing, changed or longer object is ErrCorrupt.
func TestReadIconChecked(t *testing.T) {
	e := newEnv(t, blob.Options{})
	data := []byte("an icon's bytes")
	sha, _, err := e.svc.PutIcon(ctx, "default", data)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.ReadIcon(ctx, "default", sha, int64(len(data))); err != nil || string(got) != string(data) {
		t.Fatalf("a read: %q %v", got, err)
	}
	putObject(t, e.fsStore, blob.IconKey(sha), []byte("an icon's bytez"))
	if _, err := e.svc.ReadIcon(ctx, "default", sha, int64(len(data))); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("a changed object: %v", err)
	}
	putObject(t, e.fsStore, blob.IconKey(sha), append(append([]byte{}, data...), '!'))
	if _, err := e.svc.ReadIcon(ctx, "default", sha, int64(len(data))); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("a longer object: %v", err)
	}
	if err := e.fsStore.Delete(ctx, blob.IconKey(sha)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReadIcon(ctx, "default", sha, int64(len(data))); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("a missing object: %v", err)
	}
}
