package blob_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// unreachable fails Stat for one key, as a store that cannot be reached would (spec 0018: unreadable).
type unreachable struct {
	blob.Store
	mu  sync.Mutex
	key string
}

func (f *unreachable) Stat(ctx context.Context, key string) (int64, error) {
	f.mu.Lock()
	k := f.key
	f.mu.Unlock()
	if key == k {
		return 0, errors.New("connection reset")
	}
	return f.Store.Stat(ctx, key)
}

// Verifying stored bodies (spec 0018): every outcome, quick and full, marking once, concurrency.
func TestVerify(t *testing.T) {
	st := openStore(t)
	fsStore := openFS(t, filepath.Join(t.TempDir(), "blobs"))
	fl := &unreachable{Store: fsStore}
	svc, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: fl}},
		blob.Options{MaxBody: 64 << 20, MaxIngests: 4, SpoolDir: filepath.Join(t.TempDir(), "spool"), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	e := &env{st: st, svc: svc, fsStore: fsStore}

	recs := map[string]store.Blob{}
	for i, name := range []string{"ok", "missing", "short", "long", "middle", "last", "record", "unreadable"} {
		recs[name] = e.commit(t, extension(t, 2<<20+512<<10, uint64(100+i))) // three chunks, the last partial
	}
	obj := func(name string) (string, []byte) {
		key := blob.StreamKey(recs[name].StreamHash)
		return key, readObject(t, fsStore, key)
	}
	key, _ := obj("missing")
	if err := fsStore.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	key, b := obj("short")
	putObject(t, fsStore, key, b[:len(b)-1])
	key, b = obj("long")
	putObject(t, fsStore, key, append(b, 0))
	key, b = obj("middle")
	b[1<<20+7] ^= 1
	putObject(t, fsStore, key, b)
	key, b = obj("last")
	b[len(b)-1] ^= 1
	putObject(t, fsStore, key, b)
	if err := store.ExecRaw(ctx, st, "UPDATE blobs SET stream_chunks = ? WHERE body_hash = ?", []byte{1, 2, 3}, recs["record"].BodyHash); err != nil {
		t.Fatal(err)
	}
	fl.key = blob.StreamKey(recs["unreadable"].StreamHash)

	pass := func(o blob.VerifyOptions) (blob.VerifyResult, map[string]blob.Problem) {
		t.Helper()
		got := map[string]blob.Problem{}
		o.Problem = func(p blob.Problem) {
			for n, r := range recs {
				if r.BodyHash == p.BodyHash {
					got[n] = p
				}
			}
		}
		r, err := svc.Verify(ctx, "default", o)
		if err != nil {
			t.Fatal(err)
		}
		return r, got
	}
	outcomes := func(got map[string]blob.Problem) map[string]string {
		out := map[string]string{}
		for n, p := range got {
			out[n] = p.Outcome
		}
		return out
	}
	same := func(a, b map[string]string) bool {
		if len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if b[k] != v {
				return false
			}
		}
		return true
	}

	// quick: sizes only; a chunk changed in place is not seen
	r, got := pass(blob.VerifyOptions{})
	quick := map[string]string{"missing": "missing", "short": "short", "long": "long", "record": "record", "unreadable": "unreadable"}
	if !same(outcomes(got), quick) || r.Checked != 8 || r.OK != 3 || r.Clean() || r.Bytes != 0 || r.Marked != 0 {
		t.Fatalf("quick: %+v %v", r, outcomes(got))
	}
	// full, at once and one at a time: the changed chunks too, each named
	full := map[string]string{"missing": "missing", "short": "short", "long": "long", "middle": "corrupt", "last": "corrupt", "record": "record",
		"unreadable": "unreadable"}
	for _, n := range []int{1, 8} {
		r, got = pass(blob.VerifyOptions{Full: true, Concurrency: n})
		if !same(outcomes(got), full) || r.OK != 1 || r.Bytes == 0 {
			t.Fatalf("full (%d): %+v %v", n, r, outcomes(got))
		}
		if got["middle"].Detail != "chunk 1" || got["last"].Detail != "chunk 2" {
			t.Fatalf("chunks: %q %q", got["middle"].Detail, got["last"].Detail)
		}
	}
	// nothing marked without -mark; with it, each problem but a damaged record and a transport error, once
	for _, n := range []string{"missing", "middle"} {
		if b, _ := st.GetBlob(ctx, "default", recs[n].BodyHash); !b.CorruptAt.IsZero() {
			t.Fatalf("%s marked without -mark", n)
		}
	}
	r, got = pass(blob.VerifyOptions{Full: true, Mark: true})
	var marked []string
	for n, p := range got {
		if p.Marked {
			marked = append(marked, n)
		}
	}
	slices.Sort(marked)
	if !slices.Equal(marked, []string{"last", "long", "middle", "missing", "short"}) || r.Marked != 5 {
		t.Fatalf("marked: %v %+v", marked, r)
	}
	if b, _ := st.GetBlob(ctx, "default", recs["middle"].BodyHash); b.CorruptAt.IsZero() {
		t.Fatal("corrupt_at not set")
	}
	r, got = pass(blob.VerifyOptions{Full: true, Mark: true})
	if r.Marked != 0 || !got["middle"].WasMarked || got["middle"].Marked {
		t.Fatalf("a second pass marks again: %+v %+v", r, got["middle"])
	}
	// the next commit of a body repairs it: verified ok again
	e.commit(t, extension(t, 2<<20+512<<10, 104)) // "middle"
	if _, got = pass(blob.VerifyOptions{Full: true}); got["middle"].Outcome != "" {
		t.Fatalf("repaired: %+v", got["middle"])
	}
	if _, err := svc.Verify(ctx, "default", blob.VerifyOptions{Concurrency: 33}); err == nil {
		t.Fatal("concurrency 33 accepted")
	}
	if _, err := svc.Verify(ctx, "nope", blob.VerifyOptions{}); err == nil {
		t.Fatal("an unknown domain accepted")
	}
}
