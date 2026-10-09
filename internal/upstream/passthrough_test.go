package upstream_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// passthrough makes the fake repository DuckDB's core repository, feeding the passthrough channel.
func (en *env) passthrough(t *testing.T, names ...string) *serve.Handler {
	t.Helper()
	en.up.DuckDB = upstream.DuckDB{CoreURL: en.repo.srv.URL, CoreKeys: []*rsa.PublicKey{&en.key.PublicKey}}
	en.rel.CoreKeys = en.up.DuckDB.CoreKeys
	must(t, func() error {
		_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "mirror", []string{"v2.0.0"}, nil)
		return err
	})
	var entries []store.UpstreamEntry
	for _, n := range names {
		entries = append(entries, store.UpstreamEntry{Name: n})
	}
	u, err := en.up.Add(ctx, admin, "acme", upstream.Spec{Name: "core", Kind: store.UpstreamCore, Channel: "mirror",
		Platforms: []string{"linux_amd64"}, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if u.Visibility != store.Public {
		t.Fatalf("a passthrough upstream's visibility: %s", u.Visibility)
	}
	h := serve.NewHandler(en.st, en.ks, en.blob, serve.Options{MaxDownloads: 8, MaxDownloadsPerClient: 8, MinRate: 1024,
		WriteIdleTimeout: 10 * time.Second, Log: slog.New(slog.DiscardHandler), PublicURL: "https://kista.example",
		Auths: &auth.TenantAuths{Store: en.st}, Snapshots: &release.Snapshots{Store: en.st}})
	h.SetReady(true)
	return h
}

func (en *env) syncCore(t *testing.T) upstream.Result {
	t.Helper()
	if _, err := en.up.Sync(ctx, admin, "acme", "core", false); err != nil {
		t.Fatal(err)
	}
	en.run.Once(ctx)
	u, _ := en.up.Get(ctx, admin, "acme", "core")
	var r upstream.Result
	mustJSON(t, u.LastRun, &r)
	return r
}

func fetch(t *testing.T, h http.Handler, method, path string) (int, []byte, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), rec.Header()
}

func TestPassthrough(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		file := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "json_duckdb_cpp_init")
		en.repo.put("v2.0.0", "linux_amd64", "json", file)
		h := en.passthrough(t, "json")
		if r := en.syncCore(t); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the run: %+v", r)
		}
		// DuckDB's file, its own signature: identical once inflated, over plain http, GET and HEAD
		const P = "/acme/mirror/v2.0.0/linux_amd64/json.duckdb_extension"
		code, body, hd := fetch(t, h, "GET", P)
		if code != 200 || !bytes.Equal(body, file) || hd.Get("Cache-Control") != "public, no-cache, no-transform" {
			t.Fatalf("plain: %d %d bytes %v", code, len(body), hd)
		}
		code, gz, hd := fetch(t, h, "GET", P+".gz")
		if code != 200 || hd.Get("ETag") == "" {
			t.Fatalf("gz: %d", code)
		}
		if got := gunzip(t, gz); !bytes.Equal(got, file) {
			t.Fatal("the gz answer is not DuckDB's file")
		}
		if code, body, _ := fetch(t, h, "HEAD", P+".gz"); code != 200 || len(body) != 0 {
			t.Fatalf("HEAD: %d", code)
		}
		if code, _, _ := fetch(t, h, "GET", "/acme/mirror/v2.0.0/linux_amd64/nope.duckdb_extension.gz"); code != 404 {
			t.Fatalf("a miss: %d", code)
		}
		// over https with a token: the token is never looked at, the answer is the same
		req := serve.WithHTTPS(httptest.NewRequest("GET", P+".gz", nil))
		req.Header.Set("Authorization", "Bearer not-a-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), gz) || rec.Header().Get("Vary") != "" {
			t.Fatalf("https: %d %v", rec.Code, rec.Header())
		}
		// no signature rows: DuckDB's signature is the release's
		var sigs int
		if err := store.QueryRowRaw(ctx, en.st, "SELECT COUNT(*) FROM release_signatures s JOIN channels c ON c.id = s.channel_id WHERE c.kind = 'passthrough'", &sigs); err != nil || sigs != 0 {
			t.Fatalf("signature rows in a passthrough channel: %d %v", sigs, err)
		}
		// its releases stay public
		pr, err := en.rel.List(ctx, admin, "acme", "mirror", "json")
		if err != nil || len(pr) != 1 {
			t.Fatal(pr, err)
		}
		if _, err := en.rel.Apply(ctx, admin, "acme", "mirror", "json", pr[0].ID, release.SetPrivate, 0); !errors.Is(err, release.ErrState) {
			t.Fatalf("a private passthrough release: %v", err)
		}
		if code, _, _ := fetch(t, h, "GET", "/acme/mirror/.well-known/duckdb-extension-repo.json"); code != 404 {
			t.Fatalf(".well-known: %d", code)
		}
		// the tenant's own json in a signed channel shadows DuckDB's; a yank does not lift it
		repl := signed(t, en.key, cpp("9.0", "v2.0.0"), 7, "json_duckdb_cpp_init")
		rel, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(repl), release.AddOptions{Name: "json"})
		if err != nil {
			t.Fatal(err)
		}
		if code, _, _ := fetch(t, h, "GET", P+".gz"); code != 404 {
			t.Fatalf("a shadowed name: %d", code)
		}
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "json", rel.ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := fetch(t, h, "GET", P+".gz"); code != 404 {
			t.Fatalf("a yanked replacement lifted the shadow: %d", code)
		}
		if xs, err := en.up.Shadows(ctx, admin, "acme"); err != nil || len(xs) != 1 || xs[0].Name != "json" {
			t.Fatalf("shadows: %+v %v", xs, err)
		}
		if err := en.up.RemoveShadow(ctx, admin, "acme", "json"); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := fetch(t, h, "GET", P+".gz"); code != 200 {
			t.Fatalf("after the shadow's removal: %d", code)
		}
		// a block reaches passthrough channels
		hb, _, _ := extfile.HashBody(bytes.NewReader(file[:len(file)-extfile.SignatureSize]), 1<<30)
		if _, _, err := en.rel.Block(ctx, admin, "acme", hb.String(), "CVE"); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := fetch(t, h, "GET", P+".gz"); code != 404 {
			t.Fatalf("a blocked body: %d", code)
		}
		// a passthrough channel's releases are public; its upstreams core only
		if _, err := en.up.Set(ctx, admin, "acme", "core", store.Private, "", 0); err == nil {
			t.Fatal("a private passthrough upstream")
		}
	})
}

// Replacements released before the first passthrough upstream become shadows when it is added,
// once per tenant: an administrator's removal stays.
func TestShadowBackfill(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		repl := signed(t, en.key, cpp("9.0", "v2.0.0"), 7, "json_duckdb_cpp_init")
		if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(repl), release.AddOptions{Name: "json"}); err != nil {
			t.Fatal(err)
		}
		must(t, func() error { return en.up.RemoveShadow(ctx, admin, "acme", "json") }) // as before 1b
		en.passthrough(t, "json")
		if xs, _ := en.up.Shadows(ctx, admin, "acme"); len(xs) != 1 || xs[0].Name != "json" {
			t.Fatalf("backfilled shadows: %+v", xs)
		}
		must(t, func() error { return en.up.RemoveShadow(ctx, admin, "acme", "json") })
		must(t, func() error { return en.up.Remove(ctx, admin, "acme", "core", 0) })
		if _, err := en.up.Add(ctx, admin, "acme", upstream.Spec{Name: "core", Kind: store.UpstreamCore, Channel: "mirror",
			Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "json"}}}); err != nil {
			t.Fatal(err)
		}
		if xs, _ := en.up.Shadows(ctx, admin, "acme"); len(xs) != 0 {
			t.Fatalf("a removed shadow came back: %+v", xs)
		}
		// asking for a private passthrough upstream is refused
		if _, err := en.up.Add(ctx, admin, "acme", upstream.Spec{Name: "core2", Kind: store.UpstreamCore, Channel: "mirror",
			Platforms: []string{"linux_amd64"}, Visibility: store.Private}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a private passthrough upstream: %v", err)
		}
	})
}

// A body other than DuckDB's core build under a core name shadows it, from whatever source; DuckDB's
// own build does not.
func TestShadowSources(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.passthrough(t, "json")
		// DuckDB's json mirrored into a signed channel too: no shadow
		must(t, func() error {
			_, err := en.up.Add(ctx, admin, "acme", upstream.Spec{Name: "core-signed", Kind: store.UpstreamCore, Channel: "other",
				Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "json"}}})
			return err
		})
		en.repo.put("v2.0.0", "linux_amd64", "json", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "json_duckdb_cpp_init"))
		if _, err := en.up.Sync(ctx, admin, "acme", "core-signed", false); err != nil {
			t.Fatal(err)
		}
		en.run.Once(ctx)
		if rs := en.releasesIn(t, "other", "json"); len(rs) != 1 {
			t.Fatalf("DuckDB's json in a signed channel: %+v", rs)
		}
		if xs, _ := en.up.Shadows(ctx, admin, "acme"); len(xs) != 0 {
			t.Fatalf("DuckDB's own build shadowed itself: %+v", xs)
		}
		// another repository's httpfs (allow_reserved) in a signed channel shadows DuckDB's httpfs
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		hp := signed(t, other, cpp("1.0", "v2.0.0"), 3, "httpfs_duckdb_cpp_init")
		other2 := newRepo(t)
		der, _ := x509.MarshalPKIXPublicKey(&other.PublicKey)
		other2.keys = []string{base64.StdEncoding.EncodeToString(der)}
		other2.put("v2.0.0", "linux_amd64", "httpfs", hp)
		if _, err := en.up.Add(ctx, admin, "acme", upstream.Spec{Name: "theirs", Kind: store.UpstreamRepo, Prefix: other2.srv.URL,
			Keys: []string{extfile.Fingerprint(&other.PublicKey)}, Channel: "prod", Platforms: []string{"linux_amd64"},
			Entries: []store.UpstreamEntry{{Name: "httpfs", AllowReserved: true}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.Sync(ctx, admin, "acme", "theirs", false); err != nil {
			t.Fatal(err)
		}
		en.run.Once(ctx)
		if xs, _ := en.up.Shadows(ctx, admin, "acme"); len(xs) != 1 || xs[0].Name != "httpfs" {
			t.Fatalf("a foreign httpfs: %+v", xs)
		}
	})
}

// A mirror made due during its run (a new entry) is due again when the run ends.
func TestDueDuringRun(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.up.Config.Interval = time.Hour
		p := cellPath("v2.0.0", "linux_amd64", "tresor")
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		gate := make(chan struct{})
		en.repo.mu.Lock()
		en.repo.gate[p] = gate
		en.repo.mu.Unlock()
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { en.run.Once(ctx); close(done) }()
		<-en.repo.hit
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "other"}); err != nil {
			t.Fatal(err)
		}
		close(gate)
		<-done
		u, _ := en.up.Get(ctx, admin, "acme", "acme-repo")
		if u.NextRunAt.After(time.Now()) {
			t.Fatalf("the run overwrote a due mark: next run %v", u.NextRunAt)
		}
	})
}

func TestSchedule(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.up.Config.Interval = time.Hour
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
			t.Fatal(err)
		}
		p := cellPath("v2.0.0", "linux_amd64", "tresor")
		en.run.Once(ctx) // a new mirror is due
		if g, _ := en.repo.count(p); g != 1 {
			t.Fatalf("the first scheduled run: %d", g)
		}
		u, _ := en.up.Get(ctx, admin, "acme", "acme-repo")
		if d := time.Until(u.NextRunAt); d < 55*time.Minute || d > 67*time.Minute {
			t.Fatalf("the next run in %v", d)
		}
		en.run.Once(ctx) // not due
		if g, _ := en.repo.count(p); g != 1 {
			t.Fatal("a run before it was due")
		}
		// a new entry, and a new DuckDB version of the channel, make it due
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "other"}); err != nil {
			t.Fatal(err)
		}
		en.run.Once(ctx)
		if g, _ := en.repo.count(p); g != 2 {
			t.Fatal("a new entry did not make it due")
		}
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.1.0"}, nil)
			return err
		})
		en.run.Once(ctx)
		if g, _ := en.repo.count(p); g != 3 {
			t.Fatal("a new DuckDB version did not make it due")
		}
		// a dry run leaves the schedule
		before, _ := en.up.Get(ctx, admin, "acme", "acme-repo")
		en.sync(t, true)
		after, _ := en.up.Get(ctx, admin, "acme", "acme-repo")
		if !after.NextRunAt.Equal(before.NextRunAt) {
			t.Fatalf("a dry run moved the schedule: %v -> %v", before.NextRunAt, after.NextRunAt)
		}
	})
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzipReader(b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
