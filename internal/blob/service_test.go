package blob_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

var ctx = context.Background()

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Cleanup()
	os.Exit(code)
}

// extension returns body ‖ sig: n incompressible bytes, the metadata prefix and block, and a fake
// signature (the blob service never checks it; callers do).
func extension(t *testing.T, n int, seed uint64) []byte {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: extfile.ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, extfile.MetadataPrefix...)
	b = append(b, block[:]...)
	return append(b, bytes.Repeat([]byte{0x5a}, extfile.SignatureSize)...)
}

type env struct {
	st      *store.Store
	svc     *blob.Service
	fsStore *fs.Store
	dir     string
	spool   string
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func openFS(t *testing.T, dir string) *fs.Store {
	t.Helper()
	s, err := fs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newEnv(t *testing.T, o blob.Options) *env {
	t.Helper()
	e := &env{st: openStore(t), dir: filepath.Join(t.TempDir(), "blobs"), spool: filepath.Join(t.TempDir(), "spool")}
	e.fsStore = openFS(t, e.dir)
	if o.MaxBody == 0 {
		o.MaxBody = 64 << 20
	}
	if o.MaxIngests == 0 {
		o.MaxIngests = 4
	}
	o.SpoolDir = e.spool
	o.Log = slog.New(slog.DiscardHandler)
	svc, err := blob.NewService(ctx, e.st, []blob.Domain{{Name: "default", Kind: "fs", Store: e.fsStore}}, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	e.svc = svc
	return e
}

func (e *env) commit(t *testing.T, file []byte) store.Blob {
	t.Helper()
	sp, err := e.svc.Spool(ctx, "default", bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	rec, err := sp.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (e *env) spoolFiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(e.spool)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func readAll(t *testing.T, r io.Reader) ([]byte, error) {
	t.Helper()
	return io.ReadAll(r)
}

func sig(b byte) []byte { return bytes.Repeat([]byte{b}, extfile.SignatureSize) }

// gzipOf is spec 0002's WriteGzip output for rec's body with sig.
func gzipOf(t *testing.T, file []byte, s []byte) []byte {
	t.Helper()
	body := file[:len(file)-extfile.SignatureSize]
	var z bytes.Buffer
	pre, err := extfile.Precompress(bytes.NewReader(body), &z, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := extfile.WriteGzip(&out, pre, &z, pre.BodyHash, s); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestCommitAndServeGzip(t *testing.T) {
	e := newEnv(t, blob.Options{})
	file := extension(t, 2<<20+12345, 1)
	rec := e.commit(t, file)
	if e.spoolFiles(t) != 0 {
		t.Fatal("spool files left after commit")
	}
	if n, err := e.fsStore.Stat(ctx, blob.StreamKey(rec.StreamHash)); err != nil || n != rec.StreamLen {
		t.Fatalf("stored stream: %d %v", n, err)
	}
	if rec.StreamLen <= 2<<20 {
		t.Fatalf("test needs a stream over two chunks, got %d", rec.StreamLen)
	}
	got, err := e.svc.Record(ctx, "default", mustHash(t, rec.BodyHash))
	if err != nil || got.StreamHash != rec.StreamHash {
		t.Fatalf("record: %+v %v", got, err)
	}

	s := sig(7)
	f, err := e.svc.OpenGzip(ctx, rec, s)
	if err != nil {
		t.Fatal(err)
	}
	want := gzipOf(t, file, s)
	if f.Size != int64(len(want)) || f.ContentType != "application/gzip" || !strings.HasPrefix(f.ETag, `"`) {
		t.Fatalf("file: %+v, want size %d", f, len(want))
	}
	r := f.NewReader(ctx)
	b, err := readAll(t, r)
	r.Close()
	if err != nil || !bytes.Equal(b, want) {
		t.Fatalf("gzip bytes differ from WriteGzip (%v)", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(plain, append(append([]byte{}, file[:len(file)-256]...), s...)) {
		t.Fatalf("gunzip: %v", err)
	}

	// ranges: inside a chunk, across chunks, the last bytes, the header and the tail
	const mib = 1 << 20
	for _, rg := range [][2]int64{{0, 5}, {10, 100}, {mib + 5, 10}, {mib, mib + 20}, {mib + 10 - 3, 6}, {f.Size - 300, 300}, {f.Size - 1, 1}, {3, 2*mib + 1000}} {
		r := f.NewReader(ctx)
		if _, err := r.Seek(rg[0], io.SeekStart); err != nil {
			t.Fatal(err)
		}
		part, err := io.ReadAll(io.LimitReader(r, rg[1]))
		r.Close()
		if err != nil || !bytes.Equal(part, want[rg[0]:rg[0]+rg[1]]) {
			t.Fatalf("range %v: %v", rg, err)
		}
	}
	// a reader that seeks back and forth
	r = f.NewReader(ctx)
	defer r.Close()
	for _, off := range []int64{2 * mib, 5, mib + 50, 2*mib + 1, 0} {
		r.Seek(off, io.SeekStart)
		var one [64]byte
		n, err := io.ReadFull(r, one[:])
		if err != nil || !bytes.Equal(one[:n], want[off:off+int64(n)]) {
			t.Fatalf("seek %d: %v", off, err)
		}
	}
	// another signature, another etag; another kind, another etag
	f2, _ := e.svc.OpenGzip(ctx, rec, sig(8))
	p, _ := e.svc.OpenPlain(ctx, rec, s)
	if f2.ETag == f.ETag || p.ETag == f.ETag {
		t.Fatal("etags collide")
	}
}

func mustHash(t *testing.T, s string) extfile.BodyHash {
	t.Helper()
	var h extfile.BodyHash
	if len(s) != 64 {
		t.Fatal("hash")
	}
	for i := range h {
		var b byte
		for _, c := range s[2*i : 2*i+2] {
			b <<= 4
			if c >= 'a' {
				b |= byte(c-'a') + 10
			} else {
				b |= byte(c - '0')
			}
		}
		h[i] = b
	}
	return h
}

func TestServePlain(t *testing.T) {
	e := newEnv(t, blob.Options{})
	file := extension(t, 1<<20+77, 2)
	rec := e.commit(t, file)
	s := sig(9)
	f, err := e.svc.OpenPlain(ctx, rec, s)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, file[:len(file)-256]...), s...)
	if f.Size != int64(len(want)) || f.ContentType != "application/octet-stream" {
		t.Fatalf("plain: %+v", f)
	}
	r := f.NewReader(ctx)
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(b, want) {
		t.Fatalf("plain bytes differ: %v", err)
	}
	// seeks, including into the signature and backwards
	r = f.NewReader(ctx)
	defer r.Close()
	for _, off := range []int64{f.Size - 10, 100, int64(len(want)) - 256, 1 << 20, 0} {
		r.Seek(off, io.SeekStart)
		part, err := io.ReadAll(io.LimitReader(r, 50))
		end := min(off+50, f.Size)
		if err != nil || !bytes.Equal(part, want[off:end]) {
			t.Fatalf("plain seek %d: %v", off, err)
		}
	}
	// a record whose length is wrong is refused at the end of the body
	bad := rec
	bad.BodyLen--
	fb, _ := e.svc.OpenPlain(ctx, bad, s)
	rb := fb.NewReader(ctx)
	if _, err := io.ReadAll(rb); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("short record: %v", err)
	}
	rb.Close()
	bad.BodyLen += 2
	fb, _ = e.svc.OpenPlain(ctx, bad, s)
	rb = fb.NewReader(ctx)
	if _, err := io.ReadAll(rb); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("long record: %v", err)
	}
	rb.Close()
}

func TestTamperedObject(t *testing.T) {
	e := newEnv(t, blob.Options{})
	file := extension(t, 2<<20, 3)
	rec := e.commit(t, file)
	key := blob.StreamKey(rec.StreamHash)
	obj := readObject(t, e.fsStore, key)
	obj[1<<20+100] ^= 1 // chunk 1
	putObject(t, e.fsStore, key, obj)

	f, _ := e.svc.OpenGzip(ctx, rec, sig(1))
	want := gzipOf(t, file, sig(1))
	r := f.NewReader(ctx)
	got, err := io.ReadAll(r)
	r.Close()
	if !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("tampered: %v", err)
	}
	// exactly the header and chunk 0 were returned, nothing of chunk 1
	if len(got) != 10+1<<20 || !bytes.Equal(got, want[:len(got)]) {
		t.Fatalf("returned %d bytes", len(got))
	}
	stored, err := e.st.GetBlob(ctx, "default", rec.BodyHash)
	if err != nil || stored.CorruptAt.IsZero() {
		t.Fatalf("corrupt_at not set: %+v %v", stored, err)
	}
	// plain fails too
	p, _ := e.svc.OpenPlain(ctx, rec, sig(1))
	pr := p.NewReader(ctx)
	if _, err := io.ReadAll(pr); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("plain over a tampered object: %v", err)
	}
	pr.Close()

	// the next commit of the body repairs the object and clears corrupt_at
	rec2 := e.commit(t, file)
	if rec2.StreamHash != rec.StreamHash || !rec2.CorruptAt.IsZero() {
		t.Fatalf("recommit: %+v", rec2)
	}
	if !bytes.Equal(gzipBytes(t, e.svc, rec2), want) {
		t.Fatal("not repaired")
	}

	// a missing object is ErrCorrupt as well, and a short one
	if err := e.fsStore.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	r = f.NewReader(ctx)
	if _, err := io.ReadAll(r); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("missing object: %v", err)
	}
	r.Close()
	putObject(t, e.fsStore, key, obj[:len(obj)-1])
	r = f.NewReader(ctx)
	if _, err := io.ReadAll(r); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("short object: %v", err)
	}
	r.Close()
}

func gzipBytes(t *testing.T, svc *blob.Service, rec store.Blob) []byte {
	t.Helper()
	f, err := svc.OpenGzip(ctx, rec, sig(1))
	if err != nil {
		t.Fatal(err)
	}
	r := f.NewReader(ctx)
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readObject(t *testing.T, s blob.Store, key string) []byte {
	t.Helper()
	r, err := s.Get(ctx, key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func putObject(t *testing.T, s blob.Store, key string, b []byte) {
	t.Helper()
	if err := s.Put(ctx, key, bytes.NewReader(b), int64(len(b))); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedSpoolStoresNothing(t *testing.T) {
	e := newEnv(t, blob.Options{})
	sp, err := e.svc.Spool(ctx, "default", bytes.NewReader(extension(t, 1000, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if sp.File().Metadata.Platform != "linux_amd64" {
		t.Fatalf("metadata: %+v", sp.File().Metadata)
	}
	sp.Close()
	sp.Close()
	if e.spoolFiles(t) != 0 {
		t.Fatal("spool left behind")
	}
	n := 0
	_ = e.fsStore.List(ctx, "streams/", func(string, int64, time.Time) error { n++; return nil })
	if n != 0 {
		t.Fatal("a rejected spool stored a stream")
	}
	if _, err := e.svc.Spool(ctx, "nope", bytes.NewReader(nil)); !errors.Is(err, blob.ErrDomain) {
		t.Fatalf("unknown domain: %v", err)
	}
	if _, err := e.svc.Spool(ctx, "default", bytes.NewReader([]byte("not an extension"))); err == nil {
		t.Fatal("spooled garbage")
	}
	if e.spoolFiles(t) != 0 {
		t.Fatal("spool left behind after a parse failure")
	}
}

func TestLimits(t *testing.T) {
	e := newEnv(t, blob.Options{MaxBody: 4096, MaxIngests: 1})
	if _, err := e.svc.Spool(ctx, "default", bytes.NewReader(extension(t, 5000, 5))); !errors.Is(err, blob.ErrTooLarge) {
		t.Fatalf("over max_body: %v", err)
	}
	if e.spoolFiles(t) != 0 {
		t.Fatal("spool left behind after ErrTooLarge")
	}
	sp, err := e.svc.Spool(ctx, "default", bytes.NewReader(extension(t, 1000, 5)))
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := e.svc.Spool(short, "default", bytes.NewReader(extension(t, 1000, 5))); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("max_ingests not enforced: %v", err)
	}
	sp.Close()
	sp2, err := e.svc.Spool(ctx, "default", bytes.NewReader(extension(t, 1000, 5)))
	if err != nil {
		t.Fatalf("slot not released: %v", err)
	}
	sp2.Close()
}

func TestSweepAndSpoolDir(t *testing.T) {
	st := openStore(t)
	dir := filepath.Join(t.TempDir(), "blobs")
	spool := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "spool-leftover"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	domains := func() []blob.Domain { return []blob.Domain{{Name: "default", Kind: "fs", Store: openFS(t, dir)}} }
	svc, err := blob.NewService(ctx, st, domains(), blob.Options{SpoolDir: spool, MaxBody: 1 << 20, MaxIngests: 1})
	if err != nil {
		t.Fatal(err)
	}
	svc.Close()
	if _, err := os.Stat(filepath.Join(spool, "spool-leftover")); !os.IsNotExist(err) {
		t.Fatal("leftover not swept")
	}
	if err := os.Chmod(spool, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := blob.NewService(ctx, st, domains(), blob.Options{SpoolDir: spool, MaxBody: 1 << 20, MaxIngests: 1}); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("accepted a spool_dir others can enter: %v", err)
	}
	if _, err := blob.NewService(ctx, st, domains(), blob.Options{SpoolDir: "spool", MaxBody: 1 << 20, MaxIngests: 1}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("accepted a relative spool_dir: %v", err)
	}
}

func TestConcurrentCommits(t *testing.T) {
	e := newEnv(t, blob.Options{})
	file := extension(t, 300000, 6)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	recs := make([]store.Blob, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sp, err := e.svc.Spool(ctx, "default", bytes.NewReader(file))
			if err != nil {
				errs[i] = err
				return
			}
			defer sp.Close()
			recs[i], errs[i] = sp.Commit(ctx)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		if recs[i].StreamHash != recs[0].StreamHash {
			t.Fatal("racing commits disagree")
		}
	}
}

// A dedup hit uploads anyway: a planted object is replaced.
func TestDedupHitUploads(t *testing.T) {
	e := newEnv(t, blob.Options{})
	file := extension(t, 5000, 7)
	rec := e.commit(t, file)
	key := blob.StreamKey(rec.StreamHash)
	putObject(t, e.fsStore, key, []byte("planted"))
	e.commit(t, file)
	if n, _ := e.fsStore.Stat(ctx, key); n != rec.StreamLen {
		t.Fatalf("planted object kept: %d bytes", n)
	}
}

// public wraps a store that answers anonymous reads.
type public struct{ blob.Store }

func (public) Anonymous(context.Context, string) (bool, bool) { return true, true }

func TestDomains(t *testing.T) {
	st := openStore(t)
	base := t.TempDir()
	// the service owns (and closes) its stores, so every service gets freshly opened ones
	at := func(rel string) blob.Store { return openFS(t, filepath.Join(base, rel)) }
	opts := func() blob.Options {
		return blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 1 << 20, MaxIngests: 1}
	}
	newSvc := func(d ...blob.Domain) error {
		svc, err := blob.NewService(ctx, st, d, opts())
		if err == nil {
			svc.Close()
		}
		return err
	}
	if err := newSvc(blob.Domain{Name: "default", Kind: "fs", Store: at("a")}); err != nil {
		t.Fatal(err)
	}
	if err := newSvc(blob.Domain{Name: "default", Kind: "fs", Store: at("a")}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	// the same name now pointing elsewhere
	if err := newSvc(blob.Domain{Name: "default", Kind: "fs", Store: at("b")}); !errors.Is(err, blob.ErrDomain) {
		t.Fatalf("re-pointed domain: %v", err)
	}
	// overlapping stores
	if err := newSvc(blob.Domain{Name: "default", Kind: "fs", Store: at("a")}, blob.Domain{Name: "x", Kind: "fs", Store: at("a/inner")}); !errors.Is(err, blob.ErrDomain) {
		t.Fatalf("overlap: %v", err)
	}
	if err := newSvc(blob.Domain{Name: "default", Kind: "fs", Store: at("a")}, blob.Domain{Name: "ab", Kind: "fs", Store: at("ab")}); err != nil {
		t.Fatalf("a and ab do not overlap: %v", err)
	}
	// a public store
	if err := newSvc(blob.Domain{Name: "pub", Kind: "fs", Store: public{at("c")}}); !errors.Is(err, blob.ErrDomain) {
		t.Fatalf("public: %v", err)
	}
	// a store marked by another deployment
	d := at("d")
	putObject(t, d, blob.MarkerKey, []byte(`{"domain":"other","store_id":"x","deployment":"y"}`))
	if err := newSvc(blob.Domain{Name: "dd", Kind: "fs", Store: d}); !errors.Is(err, blob.ErrDomain) {
		t.Fatalf("foreign marker: %v", err)
	}
	// a second deployment (another database) refuses a store the first one marked
	if _, err := blob.NewService(ctx, openStore(t), []blob.Domain{{Name: "default", Kind: "fs", Store: at("a")}}, opts()); !errors.Is(err, blob.ErrDomain) {
		t.Fatalf("another deployment: %v", err)
	}

	// tenants: a missing domain is reported, and serving it fails closed
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		return tx.CreateTenant(ctx, &store.Tenant{Name: "acme-cn", StorageDomain: "cn"})
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: at("a")}}, opts())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	missing, err := svc.MissingDomains(ctx)
	if err != nil || len(missing) != 1 || missing[0] != "acme-cn" {
		t.Fatalf("missing: %v %v", missing, err)
	}
	if _, err := svc.Spool(ctx, "cn", bytes.NewReader(nil)); !errors.Is(err, blob.ErrDomain) {
		t.Fatalf("spool into a missing domain: %v", err)
	}
	if _, err := svc.OpenGzip(ctx, store.Blob{Domain: "cn", StreamLen: 1, StreamChunks: make([]byte, 32)}, sig(1)); !errors.Is(err, blob.ErrDomain) {
		t.Fatalf("serve from a missing domain: %v", err)
	}
}

// http.ServeContent over a File: HEAD reads nothing, Range works, ETag and type are set.
func TestServeContent(t *testing.T) {
	e := newEnv(t, blob.Options{})
	file := extension(t, 1<<20+500, 8)
	rec := e.commit(t, file)
	want := gzipOf(t, file, sig(3))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := e.svc.OpenGzip(r.Context(), rec, sig(3))
		if err != nil {
			http.Error(w, "x", 500)
			return
		}
		rd := f.NewReader(r.Context())
		defer rd.Close()
		w.Header().Set("Content-Type", f.ContentType)
		w.Header().Set("ETag", f.ETag)
		http.ServeContent(w, r, "", time.Time{}, rd)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Range", "bytes=1048570-1048600")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, want[1048570:1048601]) {
		t.Fatalf("range: %d, %d bytes", resp.StatusCode, len(body))
	}
	resp, err = http.Head(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ContentLength != int64(len(want)) || resp.Header.Get("Content-Type") != "application/gzip" || resp.Header.Get("ETag") == "" {
		t.Fatalf("head: %d %v", resp.ContentLength, resp.Header)
	}
}

// flaky cuts the first Get body after cut bytes, as a dropped connection does.
type flaky struct {
	blob.Store
	cut   int64
	fired bool
}

type cutReader struct {
	io.ReadCloser
	left int64
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	p = p[:min(int64(len(p)), c.left)]
	n, err := c.ReadCloser.Read(p)
	c.left -= int64(n)
	return n, err
}

func (f *flaky) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	r, err := f.Store.Get(ctx, key, off, n)
	if err != nil || f.fired || key == blob.MarkerKey {
		return r, err
	}
	f.fired = true
	return &cutReader{r, f.cut}, nil
}

// A connection that drops mid-object is a transport error, not corruption: nothing is marked, and
// the next reader succeeds.
func TestDroppedConnectionIsNotCorruption(t *testing.T) {
	e := newEnv(t, blob.Options{})
	file := extension(t, 2<<20, 11)
	rec := e.commit(t, file)

	fl := &flaky{Store: openFS(t, e.dir), cut: 1000}
	svc, err := blob.NewService(ctx, e.st, []blob.Domain{{Name: "default", Kind: "fs", Store: fl}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 1 << 30, MaxIngests: 1, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	f, _ := svc.OpenGzip(ctx, rec, sig(1))
	r := f.NewReader(ctx)
	_, err = io.ReadAll(r)
	r.Close()
	if err == nil || errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("a dropped connection: %v", err)
	}
	if got, _ := e.st.GetBlob(ctx, "default", rec.BodyHash); !got.CorruptAt.IsZero() {
		t.Fatal("a dropped connection marked the record corrupt")
	}
	r = f.NewReader(ctx)
	defer r.Close()
	if b, err := io.ReadAll(r); err != nil || !bytes.Equal(b, gzipOf(t, file, sig(1))) {
		t.Fatalf("after the drop: %v", err)
	}
}

// The gzip ETag follows the stream: a re-commit that makes another stream changes it, the plain
// ETag does not change.
func TestETagFollowsStream(t *testing.T) {
	e := newEnv(t, blob.Options{})
	rec := e.commit(t, extension(t, 5000, 12))
	other := rec
	other.StreamHash = strings.Repeat("0", 64)
	g1, _ := e.svc.OpenGzip(ctx, rec, sig(1))
	g2, _ := e.svc.OpenGzip(ctx, other, sig(1))
	p1, _ := e.svc.OpenPlain(ctx, rec, sig(1))
	p2, _ := e.svc.OpenPlain(ctx, other, sig(1))
	if g1.ETag == g2.ETag || p1.ETag != p2.ETag {
		t.Fatalf("etags: gz %s %s, plain %s %s", g1.ETag, g2.ETag, p1.ETag, p2.ETag)
	}
}

// Replicas (separate pools on one database) start together and commit one body concurrently, on
// every engine: Commit's retry of a lost first insert is exercised where inserts really race.
func TestReplicasRace(t *testing.T) {
	for _, eng := range storetest.Engines(t) {
		t.Run(eng.Name, func(t *testing.T) {
			const n = 4
			stores := eng.Fresh(t, n)
			if err := stores[0].Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "blobs")
			file := extension(t, 200000, 13)
			var wg sync.WaitGroup
			errs := make([]error, n)
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s, err := fs.Open(dir)
					if err != nil {
						errs[i] = err
						return
					}
					svc, err := blob.NewService(ctx, stores[i], []blob.Domain{{Name: "default", Kind: "fs", Store: s}},
						blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 1 << 20, MaxIngests: 1, Log: slog.New(slog.DiscardHandler)})
					if err != nil {
						errs[i] = err
						return
					}
					defer svc.Close()
					for range 3 {
						sp, err := svc.Spool(ctx, "default", bytes.NewReader(file))
						if err != nil {
							errs[i] = err
							return
						}
						_, err = sp.Commit(ctx)
						sp.Close()
						if err != nil {
							errs[i] = err
							return
						}
					}
				}()
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
