package blob_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// scrubStore wraps a store: Stat fails for one key, Get fails for another (Stat answering), and a hook
// runs on Stat of a third (the collector removing a record mid-pass).
type scrubStore struct {
	blob.Store
	mu                sync.Mutex
	statFail, getFail string
	hookKey           string
	hook              func()
}

func (f *scrubStore) Stat(ctx context.Context, key string) (int64, error) {
	f.mu.Lock()
	fail, hook := key == f.statFail, key == f.hookKey && f.hook != nil
	h := f.hook
	if hook {
		f.hook = nil
	}
	f.mu.Unlock()
	if fail {
		return 0, errors.New("connection reset\nby peer")
	}
	if hook {
		h()
	}
	return f.Store.Stat(ctx, key)
}

func (f *scrubStore) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	f.mu.Lock()
	fail := key == f.getFail
	f.mu.Unlock()
	if fail {
		return nil, errors.New("timeout")
	}
	return f.Store.Get(ctx, key, off, n)
}

type verifyEnv struct {
	*env
	fault *scrubStore
	recs  map[string]store.Blob
}

func newVerifyEnv(t *testing.T, names ...string) *verifyEnv {
	t.Helper()
	st := openStore(t)
	fsStore := openFS(t, filepath.Join(t.TempDir(), "blobs"))
	f := &scrubStore{Store: fsStore}
	svc, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: f}},
		blob.Options{MaxBody: 64 << 20, MaxIngests: 4, SpoolDir: filepath.Join(t.TempDir(), "spool"), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	v := &verifyEnv{env: &env{st: st, svc: svc, fsStore: fsStore}, fault: f, recs: map[string]store.Blob{}}
	for i, n := range names {
		v.recs[n] = v.commit(t, v.body(t, i)) // three chunks, the last partial
	}
	return v
}

func (v *verifyEnv) body(t *testing.T, i int) []byte { return extension(t, 2<<20+512<<10, uint64(100+i)) }

func (v *verifyEnv) key(name string) string { return blob.StreamKey(v.recs[name].StreamHash) }

// pass runs a verification and answers its result and each problem's outcome by record name.
func (v *verifyEnv) pass(t *testing.T, o blob.VerifyOptions) (blob.VerifyResult, map[string]blob.Problem, error) {
	t.Helper()
	if o.Concurrency == 0 {
		o.Concurrency = 4
	}
	got := map[string]blob.Problem{}
	o.Problem = func(p blob.Problem) {
		for n, r := range v.recs {
			if r.BodyHash == p.BodyHash {
				got[n] = p
			}
		}
	}
	r, err := v.svc.Verify(ctx, "default", o)
	return r, got, err
}

func outcomes(got map[string]blob.Problem) map[string]string {
	out := map[string]string{}
	for n, p := range got {
		out[n] = p.Outcome
	}
	return out
}

func sameOutcomes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, x := range a {
		if b[k] != x {
			return false
		}
	}
	return true
}

// Verifying stored bodies (spec 0018): every outcome, quick and full, marking as a reader would and
// only from a full read, unmarking, a record changed mid-pass skipped, paging, concurrency.
func TestVerify(t *testing.T) {
	v := newVerifyEnv(t, "ok", "missing", "short", "long", "middle", "last", "record", "unreadable", "getfail", "gone", "markedok")
	b := readObject(t, v.fsStore, v.key("short"))
	putObject(t, v.fsStore, v.key("short"), b[:len(b)-1])
	b = readObject(t, v.fsStore, v.key("long"))
	putObject(t, v.fsStore, v.key("long"), append(b, 0))
	b = readObject(t, v.fsStore, v.key("middle"))
	b[1<<20+7] ^= 1
	putObject(t, v.fsStore, v.key("middle"), b)
	b = readObject(t, v.fsStore, v.key("last"))
	b[len(b)-1] ^= 1
	putObject(t, v.fsStore, v.key("last"), b)
	if err := v.fsStore.Delete(ctx, v.key("missing")); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecRaw(ctx, v.st, "UPDATE blobs SET stream_chunks = ? WHERE body_hash = ?", []byte{1, 2, 3}, v.recs["record"].BodyHash); err != nil {
		t.Fatal(err)
	}
	if _, err := v.st.MarkBlobCorrupt(ctx, v.recs["markedok"]); err != nil { // an earlier failure; the object is intact
		t.Fatal(err)
	}
	v.fault.statFail, v.fault.getFail = v.key("unreadable"), v.key("getfail")
	// the collector removes "gone" (record and object) while the pass looks at it
	v.fault.hookKey, v.fault.hook = v.key("gone"), func() {
		if err := store.ExecRaw(ctx, v.st, "DELETE FROM blobs WHERE body_hash = ?", v.recs["gone"].BodyHash); err != nil {
			t.Error(err)
		}
		if err := v.fsStore.Delete(ctx, v.key("gone")); err != nil {
			t.Error(err)
		}
	}

	// quick: sizes only
	r, got, err := v.pass(t, blob.VerifyOptions{})
	quick := map[string]string{"missing": "missing", "short": "short", "long": "long", "record": "record", "unreadable": "unreadable", "markedok": "marked"}
	if err != nil || !sameOutcomes(outcomes(got), quick) || r.Checked != 11 || r.OK != 4 || r.Skipped != 1 || r.Clean() || r.Bytes != 0 {
		t.Fatalf("quick: %+v %v %v", r, outcomes(got), err)
	}
	if d := got["unreadable"].Detail; d != "connection reset by peer" {
		t.Fatalf("a detail on one line: %q", d)
	}
	if _, _, err := v.pass(t, blob.VerifyOptions{Mark: true}); err == nil {
		t.Fatal("marking from a quick pass")
	}

	// full, one at a time and at once, in pages of 3: the changed chunks too; a long object is read to
	// its recorded length and its chunks decide; a failing read is a transport error
	full := map[string]string{"missing": "missing", "short": "short", "long": "long", "middle": "corrupt", "last": "corrupt", "record": "record",
		"unreadable": "unreadable", "getfail": "unreadable", "markedok": "marked"}
	restore := blob.SetVerifyPage(3)
	for _, n := range []int{1, 8} {
		r, got, err = v.pass(t, blob.VerifyOptions{Full: true, Concurrency: n})
		if err != nil || !sameOutcomes(outcomes(got), full) || r.OK != 1 || r.Checked != 10 || r.Bytes == 0 || r.Marked != 0 {
			t.Fatalf("full (%d): %+v %v %v", n, r, outcomes(got), err)
		}
	}
	restore()
	if got["middle"].Detail != "chunk 1" || got["last"].Detail != "chunk 2" {
		t.Fatalf("chunks: %q %q", got["middle"].Detail, got["last"].Detail)
	}

	// -mark: what a reader would mark (missing, short, corrupt), never long, a damaged record or a
	// transport error; once
	r, got, err = v.pass(t, blob.VerifyOptions{Full: true, Mark: true})
	var marked []string
	for n, p := range got {
		if p.Marked {
			marked = append(marked, n)
		}
	}
	slices.Sort(marked)
	if err != nil || !slices.Equal(marked, []string{"last", "middle", "missing", "short"}) || r.Marked != 4 {
		t.Fatalf("marked: %v %+v %v", marked, r, err)
	}
	r, got, _ = v.pass(t, blob.VerifyOptions{Full: true, Mark: true})
	if r.Marked != 0 || !got["middle"].WasMarked {
		t.Fatalf("a second pass marks again: %+v", r)
	}

	// -unmark: a record found intact is served again; the next commit of a body repairs and clears it
	r, got, err = v.pass(t, blob.VerifyOptions{Full: true, Unmark: true})
	if err != nil || !got["markedok"].Unmarked || r.Unmarked != 1 {
		t.Fatalf("unmark: %+v %+v %v", r, got["markedok"], err)
	}
	if b, _ := v.st.GetBlob(ctx, "default", v.recs["markedok"].BodyHash); !b.CorruptAt.IsZero() {
		t.Fatal("still marked")
	}
	v.commit(t, v.body(t, 4)) // "middle"
	if _, got, _ = v.pass(t, blob.VerifyOptions{Full: true}); got["middle"].Outcome != "" || got["markedok"].Outcome != "" {
		t.Fatalf("after repair: %v", outcomes(got))
	}

	if _, _, err := v.pass(t, blob.VerifyOptions{Concurrency: 33}); err == nil {
		t.Fatal("concurrency 33 accepted")
	}
	if _, err := v.svc.Verify(ctx, "nope", blob.VerifyOptions{Concurrency: 1}); err == nil {
		t.Fatal("an unknown domain accepted")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if r, err := v.svc.Verify(cctx, "default", blob.VerifyOptions{Concurrency: 1}); !errors.Is(err, context.Canceled) || r.Unreadable != 0 {
		t.Fatalf("cancelled: %+v %v", r, err)
	}
}

// A store that lost most bodies is the wrong store, not lost bodies: nothing is marked.
func TestVerifyImplausible(t *testing.T) {
	v := newVerifyEnv(t, "a", "b", "c", "d", "e")
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := v.fsStore.Delete(ctx, v.key(n)); err != nil {
			t.Fatal(err)
		}
	}
	r, _, err := v.pass(t, blob.VerifyOptions{Full: true, Mark: true})
	if !errors.Is(err, blob.ErrImplausible) || r.Marked != 0 {
		t.Fatalf("marked a store that lost most bodies: %+v %v", r, err)
	}
	for _, n := range []string{"a", "b"} {
		if b, _ := v.st.GetBlob(ctx, "default", v.recs[n].BodyHash); !b.CorruptAt.IsZero() {
			t.Fatalf("%s marked", n)
		}
	}
	if r, _, err := v.pass(t, blob.VerifyOptions{Full: true}); err != nil || r.Missing != 4 {
		t.Fatalf("reporting without marking: %+v %v", r, err)
	}
}
