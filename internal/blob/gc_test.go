package blob_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0016 phase 1: a released body survives; a body no Build references goes in two passes a
// grace apart; an object without a row is found by the listing; tmp/ leftovers go; a dry run
// deletes nothing; a store marked for another deployment is not collected.
func TestCollector(t *testing.T) {
	e := newEnv(t, blob.Options{})
	now := time.Now().UTC()
	e.st.Now = func() time.Time { return now }
	// a tenant, a channel, a released body
	tn := store.Tenant{Name: "acme", DisplayName: "Acme"}
	ch := store.Channel{Name: "prod", Kind: store.ChannelSigned}
	if err := e.st.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.CreateTenant(ctx, &tn); err != nil {
			return err
		}
		ch.TenantID = tn.ID
		return tx.CreateChannel(ctx, &ch)
	}); err != nil {
		t.Fatal(err)
	}
	live := e.commit(t, extension(t, 3000, 1))
	b := store.Build{TenantID: tn.ID, Name: "acl", ExtVersion: "1.0", Platform: "linux_amd64", ABI: store.ABICPP, DuckDBVersion: "v2.0.0",
		BodyHash: live.BodyHash, Origin: store.OriginAdmin, CreatedBy: "os:1:t"}
	r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, Name: "acl", ExtVersion: "1.0", Platform: "linux_amd64",
		State: store.ReleaseActive, Visibility: store.Public, Seq: 1}
	if err := e.st.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
			return err
		}
		r.BuildID, r.Slot = b.ID, b.Slot()
		return tx.InsertRelease(ctx, &r, "os:1:t")
	}); err != nil {
		t.Fatal(err)
	}
	// an orphan body (no Build), an object without a row, an old tmp/ file
	orphan := e.commit(t, extension(t, 3000, 2))
	putObject(t, e.fsStore, blob.StreamKey("ab"+orphan.StreamHash[2:]), []byte("left by a crash"))
	tmp := filepath.Join(e.dir, "tmp", "0190a7b6-0000-7000-8000-000000000000")
	if err := os.WriteFile(tmp, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-48 * time.Hour)
	_ = os.Chtimes(tmp, old, old)
	c := &blob.Collector{Service: e.svc, Holder: "test", Grace: 24 * time.Hour, Log: slog.New(slog.DiscardHandler)}
	exists := func(key string) bool {
		_, err := e.fsStore.Stat(ctx, key)
		return err == nil
	}
	// within the grace: nothing but the old tmp/ file
	if r, err := c.Pass(ctx, "default", true); err != nil || r.Bodies != 0 || r.Streams != 0 || r.Tmp != 1 || r.Marked != 1 {
		t.Fatalf("a dry run within the grace: %+v %v", r, err)
	}
	if !exists("tmp/0190a7b6-0000-7000-8000-000000000000") {
		t.Fatal("a dry run deleted")
	}
	res, err := c.Pass(ctx, "default", false)
	if err != nil || res.Tmp != 1 || res.Marked != 1 || res.Streams != 0 {
		t.Fatalf("the first pass: %+v %v", res, err)
	}
	// a grace later: the orphan body's row goes and its stream is marked; the listed object is due
	now = now.Add(25 * time.Hour)
	if res, err = c.Pass(ctx, "default", false); err != nil || res.Bodies != 1 || res.Streams != 1 {
		t.Fatalf("the second pass: %+v %v", res, err)
	}
	if exists(blob.StreamKey("ab" + orphan.StreamHash[2:])) {
		t.Fatal("the listed object survived")
	}
	if !exists(blob.StreamKey(orphan.StreamHash)) {
		t.Fatal("a stream deleted in the pass that marked it")
	}
	// another grace: the orphan's stream goes; the released body stays throughout
	now = now.Add(25 * time.Hour)
	if res, err = c.Pass(ctx, "default", false); err != nil || res.Streams != 1 {
		t.Fatalf("the third pass: %+v %v", res, err)
	}
	if exists(blob.StreamKey(orphan.StreamHash)) || !exists(blob.StreamKey(live.StreamHash)) {
		t.Fatal("the wrong stream went")
	}
	if _, err := e.st.GetBlob(ctx, "default", live.BodyHash); err != nil {
		t.Fatalf("the released body's row: %v", err)
	}
	// the orphan committed again: its row and stream come back
	again := e.commit(t, extension(t, 3000, 2))
	if !exists(blob.StreamKey(again.StreamHash)) {
		t.Fatal("a commit after the collection did not upload")
	}
	// another deployment's marker: nothing is collected
	putObject(t, e.fsStore, blob.MarkerKey, []byte(`{"domain":"default","store_id":"x","deployment":"y"}`))
	if _, err := c.Pass(ctx, "default", false); err == nil {
		t.Fatal("collected a store marked for another deployment")
	}
}

// A commit meeting a stream the collector is deleting waits, then uploads it: the object is there
// when the commit returns.
func TestCommitWaitsForADelete(t *testing.T) {
	blob.SetBusyPoll(t, 10*time.Millisecond)
	e := newEnv(t, blob.Options{})
	file := extension(t, 3000, 7)
	rec := e.commit(t, file)
	stamp := deleting(t, e, rec)
	type result struct {
		rec store.Blob
		err error
	}
	done := make(chan result, 1)
	go func() {
		sp, err := e.svc.Spool(ctx, "default", bytes.NewReader(file))
		if err != nil {
			done <- result{err: err}
			return
		}
		defer sp.Close()
		r, err := sp.Commit(ctx)
		done <- result{r, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("a commit went on during the delete: %+v %v", r.rec, r.err)
	case <-time.After(200 * time.Millisecond): // twenty polls refused
	}
	if err := e.fsStore.Delete(ctx, blob.StreamKey(rec.StreamHash)); err != nil {
		t.Fatal(err)
	}
	if err := e.st.EndDelete(ctx, "default", rec.StreamHash, stamp); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if _, err := e.fsStore.Stat(ctx, blob.StreamKey(r.rec.StreamHash)); err != nil {
			t.Fatalf("the object after the commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the commit never went on")
	}
}

// deleting makes rec's stream one the collector began deleting: its row gone (no Build), its
// tombstone deleting. The store's clock is two grace periods on.
func deleting(t *testing.T, e *env, rec store.Blob) time.Time {
	t.Helper()
	now := time.Now().UTC().Add(48 * time.Hour)
	e.st.Now = func() time.Time { return now }
	ok, err := e.st.DeleteBody(ctx, "default", store.DeadBody{BodyHash: rec.BodyHash, StreamHash: rec.StreamHash}, now.Add(-time.Hour))
	if err != nil || !ok {
		t.Fatalf("delete the row: %v %v", ok, err)
	}
	now = now.Add(48 * time.Hour)
	stamp, err := e.st.BeginDelete(ctx, "default", rec.StreamHash, now.Add(-24*time.Hour), now.Add(-30*time.Minute))
	if err != nil || stamp.IsZero() {
		t.Fatalf("begin the delete: %v %v", stamp, err)
	}
	return stamp
}

// faulty is a store whose deletes a test controls.
type faulty struct {
	blob.Store
	delete func(key string) error
}

func (f *faulty) Delete(ctx context.Context, key string) error {
	if f.delete != nil {
		if err := f.delete(key); err != nil {
			return err
		}
	}
	return f.Store.Delete(ctx, key)
}

// collector is a collector over e's database and store, the store's deletes through f.
func collector(t *testing.T, e *env, f *faulty) (*blob.Collector, *[]blob.Result) {
	t.Helper()
	f.Store = e.fsStore
	svc, err := blob.NewService(ctx, e.st, []blob.Domain{{Name: "default", Kind: "fs", Store: f}},
		blob.Options{MaxBody: 64 << 20, MaxIngests: 1, SpoolDir: filepath.Join(t.TempDir(), "spool"), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	var events []blob.Result
	return &blob.Collector{Service: svc, Holder: "test", Grace: 24 * time.Hour, Log: slog.New(slog.DiscardHandler),
		Event: func(_ context.Context, r blob.Result) { events = append(events, r) }}, &events
}

// A delete the store refuses stops the pass, recorded as failed; the tombstone goes back to marked,
// so a commit of the stream goes on at once, and a later pass deletes it.
func TestRefusedDelete(t *testing.T) {
	e := newEnv(t, blob.Options{})
	rec := e.commit(t, extension(t, 3000, 8))
	now := time.Now().UTC().Add(48 * time.Hour)
	e.st.Now = func() time.Time { return now }
	f := &faulty{}
	c, events := collector(t, e, f)
	if r, err := c.Pass(ctx, "default", false); err != nil || r.Bodies != 1 { // the row goes, the stream is marked
		t.Fatalf("the first pass: %+v %v", r, err)
	}
	now = now.Add(25 * time.Hour)
	refused := errors.New("access denied")
	f.delete = func(string) error { return refused }
	if _, err := c.Pass(ctx, "default", false); !errors.Is(err, refused) {
		t.Fatalf("a refused delete: %v", err)
	}
	if ev := (*events)[len(*events)-1]; !errors.Is(ev.Err, refused) || ev.Streams != 0 {
		t.Fatalf("the failed pass's event: %+v", ev)
	}
	ts, err := e.st.DueTombstones(ctx, "default", now, now, "", 10)
	if err != nil || len(ts) != 1 || ts[0].State != "marked" {
		t.Fatalf("the tombstone after a refused delete: %+v %v", ts, err)
	}
	if _, err := e.fsStore.Stat(ctx, blob.StreamKey(rec.StreamHash)); err != nil {
		t.Fatalf("the stream after a refused delete: %v", err)
	}
	f.delete = nil
	if r, err := c.Pass(ctx, "default", false); err != nil || r.Streams != 1 {
		t.Fatalf("the pass after: %+v %v", r, err)
	}
	if _, err := e.fsStore.Stat(ctx, blob.StreamKey(rec.StreamHash)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the stream after the pass: %v", err)
	}
}

// A collector that lost its lease mid-pass stops before its next delete.
func TestLeaseLostMidPass(t *testing.T) {
	e := newEnv(t, blob.Options{})
	a := e.commit(t, extension(t, 3000, 9))
	b := e.commit(t, extension(t, 3000, 10))
	now := time.Now().UTC().Add(48 * time.Hour)
	e.st.Now = func() time.Time { return now }
	f := &faulty{}
	c, _ := collector(t, e, f)
	if r, err := c.Pass(ctx, "default", false); err != nil || r.Bodies != 2 {
		t.Fatalf("the first pass: %+v %v", r, err)
	}
	now = now.Add(25 * time.Hour)
	f.delete = func(string) error { // another replica takes the lease after the first delete
		f.delete = nil
		if err := e.st.ReleaseLease(ctx, "kista/blob/gc/default", "test"); err != nil {
			return err
		}
		if ok, err := e.st.AcquireLease(ctx, "kista/blob/gc/default", "other", time.Hour); err != nil || !ok {
			return fmt.Errorf("take the lease: %v %v", ok, err)
		}
		return nil
	}
	r, err := c.Pass(ctx, "default", false)
	if !errors.Is(err, blob.ErrLeaseLost) || r.Streams != 1 {
		t.Fatalf("a pass whose lease was taken: %+v %v", r, err)
	}
	left := 0
	for _, x := range []store.Blob{a, b} {
		if _, err := e.fsStore.Stat(ctx, blob.StreamKey(x.StreamHash)); err == nil {
			left++
		}
	}
	if left != 1 {
		t.Fatalf("%d streams left, want 1", left)
	}
}

// The resolver finishes a delete a collector left once it is stale, and only on a store marked for
// this deployment.
func TestResolve(t *testing.T) {
	e := newEnv(t, blob.Options{})
	rec := e.commit(t, extension(t, 3000, 11))
	deleting(t, e, rec)
	c, _ := collector(t, e, &faulty{})
	if r, err := c.Resolve(ctx, "default"); err != nil || r.Resolved != 0 {
		t.Fatalf("a fresh delete resolved: %+v %v", r, err)
	}
	now := e.st.Now().Add(31 * time.Minute)
	e.st.Now = func() time.Time { return now }
	if r, err := c.Resolve(ctx, "default"); err != nil || r.Resolved != 1 || r.Streams != 1 {
		t.Fatalf("a stale delete: %+v %v", r, err)
	}
	if _, err := e.fsStore.Stat(ctx, blob.StreamKey(rec.StreamHash)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the stream after the resolver: %v", err)
	}
	putObject(t, e.fsStore, blob.MarkerKey, []byte(`{"domain":"default","store_id":"x","deployment":"y"}`))
	if _, err := c.Resolve(ctx, "default"); !errors.Is(err, blob.ErrDomain) {
		t.Fatalf("resolved on a store marked for another deployment: %v", err)
	}
}

// A replica's record of a body lasts recordTTL: after, a row the collector deleted is not found.
func TestRecordCacheTTL(t *testing.T) {
	e := newEnv(t, blob.Options{})
	at := time.Now()
	blob.SetClock(t, func() time.Time { return at })
	rec := e.commit(t, extension(t, 3000, 12))
	body := mustHash(t, rec.BodyHash)
	if _, err := e.svc.Record(ctx, "default", body); err != nil {
		t.Fatal(err)
	}
	// another replica's collector deletes the row
	now := time.Now().UTC().Add(48 * time.Hour)
	e.st.Now = func() time.Time { return now }
	if ok, err := e.st.DeleteBody(ctx, "default", store.DeadBody{BodyHash: rec.BodyHash, StreamHash: rec.StreamHash}, now); err != nil || !ok {
		t.Fatalf("delete the row: %v %v", ok, err)
	}
	at = at.Add(14 * time.Minute)
	if _, err := e.svc.Record(ctx, "default", body); err != nil {
		t.Fatalf("within the TTL: %v", err)
	}
	at = at.Add(2 * time.Minute)
	if _, err := e.svc.Record(ctx, "default", body); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after the TTL: %v", err)
	}
}
