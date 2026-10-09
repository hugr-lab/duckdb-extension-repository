package upstream_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/symbols/symtest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

var (
	ctx   = context.Background()
	admin = authz.Actor{Kind: authz.ActorOS, ID: "1:test"}
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Cleanup()
	os.Exit(code)
}

// fakeRepo is an upstream DuckDB repository: files by path with ETags, and a .well-known file.
type fakeRepo struct {
	mu    sync.Mutex
	files map[string][]byte
	etags map[string]string
	keys  []string
	gets  map[string]int
	conds map[string]int // requests with If-None-Match
	n     int
	gate  map[string]chan struct{} // a path's answers wait for the gate to close
	hit   chan string              // paths of gated requests, as they arrive
	srv   *httptest.Server
}

func newRepo(t *testing.T) *fakeRepo {
	r := &fakeRepo{files: map[string][]byte{}, etags: map[string]string{}, gets: map[string]int{}, conds: map[string]int{},
		gate: map[string]chan struct{}{}, hit: make(chan string, 16)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		g := r.gate[q.URL.Path]
		r.mu.Unlock()
		if g != nil {
			r.hit <- q.URL.Path
			<-g
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.gets[q.URL.Path]++
		if q.Header.Get("If-None-Match") != "" {
			r.conds[q.URL.Path]++
		}
		if q.URL.Path == "/always304" || strings.HasSuffix(q.URL.Path, "/stale.duckdb_extension.gz") {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if q.URL.Path == "/.well-known/duckdb-extension-repo.json" {
			_ = json.NewEncoder(w).Encode(map[string]any{"signature_keys": r.keys})
			return
		}
		b, ok := r.files[q.URL.Path]
		if !ok {
			http.NotFound(w, q)
			return
		}
		etag := r.etags[q.URL.Path]
		if q.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write(b)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// put serves a file at a cell's path, gzipped, with a new ETag.
func (r *fakeRepo) put(version, platform, name string, file []byte) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(file)
	_ = zw.Close()
	r.putRaw("/"+version+"/"+platform+"/"+name+".duckdb_extension.gz", buf.Bytes())
}

func (r *fakeRepo) putRaw(path string, b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[path] = b
	r.n++
	r.etags[path] = fmt.Sprintf(`"%d-%d"`, r.n, len(b))
}

func (r *fakeRepo) touch(version, platform, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := "/" + version + "/" + platform + "/" + name + ".duckdb_extension.gz"
	r.etags[p] += "x"
}

type env struct {
	blob *blob.Service
	ks   *keys.Service
	dir  string
	st   *store.Store
	rel  *release.Service
	up   *upstream.Service
	ten  *tenants.Service
	run  *upstream.Runner
	repo *fakeRepo
	key  *rsa.PrivateKey
	fp   string
}

func newEnv(t *testing.T, e storetest.Engine) *env {
	return newEnvWith(t, e, 1<<20, egress.Config{AllowLoopbackHTTP: true})
}

func newEnvWith(t *testing.T, e storetest.Engine, maxBody int64, ec egress.Config) *env {
	t.Helper()
	st := e.Open(t)
	dir := t.TempDir()
	ks := &keys.Service{Store: st, Signers: signer.Resolver{FileDir: dir, AllowFile: true}, Authz: authz.ServerAdmin{}}
	fsStore, err := fs.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: fsStore}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: maxBody, MaxIngests: 4, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bs.Close() })
	eg, err := egress.New(ec)
	if err != nil {
		t.Fatal(err)
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	en := &env{blob: bs, ks: ks, dir: dir, st: st, repo: newRepo(t), key: k, fp: extfile.Fingerprint(&k.PublicKey)}
	en.repo.keys = []string{base64.StdEncoding.EncodeToString(der)}
	en.rel = &release.Service{Store: st, Blob: bs, Signers: ks, Authz: authz.ServerAdmin{}}
	en.ten = &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
	en.up = &upstream.Service{Store: st, Releases: en.rel, Blob: bs, Fetch: eg, Authz: authz.ServerAdmin{},
		MaxBody: maxBody, MaxIngests: 4, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	en.run = &upstream.Runner{Service: en.up, Holder: "test", Log: slog.New(slog.DiscardHandler)}
	must(t, func() error { _, err := en.ten.CreateTenant(ctx, admin, "acme", "", ""); return err })
	must(t, func() error {
		_, err := en.ten.AddVersion(ctx, admin, "v2.0.0", "release", []string{"v1.5.6", "v2.0.0"})
		return err
	})
	must(t, func() error {
		_, err := en.ten.AddVersion(ctx, admin, "v2.1.0", "release", []string{"v1.6.0", "v2.1.0"})
		return err
	})
	for name, kind := range map[string]string{"prod": store.ChannelSigned, "other": store.ChannelSigned, "mirror": store.ChannelPassthrough} {
		must(t, func() error { _, err := en.ten.CreateChannel(ctx, admin, "acme", name, kind); return err })
	}
	for _, ch := range []string{"prod", "other"} {
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", ch, []string{"v2.0.0"}, nil)
			return err
		})
		if err := signer.GenerateKeyFile(filepath.Join(dir, ch+".pem")); err != nil {
			t.Fatal(err)
		}
		must(t, func() error { _, err := ks.Add(ctx, admin, "acme", ch, "file:"+ch+".pem", true); return err })
	}
	return en
}

func must(t *testing.T, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Fatal(err)
	}
}

func each(t *testing.T, f func(t *testing.T, en *env)) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) { f(t, newEnv(t, e)) })
	}
}

func cpp(ver, duckdb string) extfile.Metadata {
	return extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: duckdb, ExtensionVersion: ver, ABI: extfile.ABICPP}
}

// signed is an extension file exporting the entry points, signed with key (nil: unsigned). A
// distinct pad makes distinct bodies.
func signed(t *testing.T, key *rsa.PrivateKey, m extfile.Metadata, pad int, exports ...string) []byte {
	t.Helper()
	block, err := extfile.EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	b := append(symtest.ELF(exports...), make([]byte, pad)...)
	b = append(b, extfile.MetadataPrefix...)
	b = append(b, block[:]...)
	sig := make([]byte, extfile.SignatureSize)
	if key != nil {
		h, _, err := extfile.HashBody(bytes.NewReader(b), 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		if sig, err = rsa.SignPKCS1v15(nil, key, crypto.SHA256, h[:]); err != nil {
			t.Fatal(err)
		}
	}
	return append(b, sig...)
}

func (en *env) spec(entries ...store.UpstreamEntry) upstream.Spec {
	return upstream.Spec{Name: "acme-repo", Kind: store.UpstreamRepo, Prefix: en.repo.srv.URL, Channel: "prod",
		Keys: []string{en.fp}, Platforms: []string{"linux_amd64"}, Entries: entries}
}

// sync requests a run and runs it on this replica.
func (en *env) sync(t *testing.T, dryRun bool) upstream.Result {
	t.Helper()
	if _, err := en.up.Sync(ctx, admin, "acme", "acme-repo", dryRun); err != nil {
		t.Fatal(err)
	}
	en.run.Once(ctx)
	u, err := en.up.Get(ctx, admin, "acme", "acme-repo")
	if err != nil {
		t.Fatal(err)
	}
	var r upstream.Result
	if err := json.Unmarshal([]byte(u.LastRun), &r); err != nil {
		t.Fatalf("last run %q: %v", u.LastRun, err)
	}
	if !u.RequestedAt.IsZero() {
		t.Fatal("the request was not taken")
	}
	return r
}

func (en *env) releases(t *testing.T, name string) []store.Candidate {
	t.Helper()
	rs, err := en.rel.List(ctx, admin, "acme", "prod", name)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func (en *env) cell(t *testing.T, name string) store.UpstreamCell {
	t.Helper()
	cs, err := en.up.Cells(ctx, admin, "acme", "acme-repo", "", [3]string{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return store.UpstreamCell{}
}

func TestMirror(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "gone"}))
		if err != nil || u.Visibility != store.Private || u.Version != 1 {
			t.Fatalf("add: %+v %v", u, err)
		}
		// a dry run checks without releasing or recording cells
		if r := en.sync(t, true); !r.DryRun || r.Counts["would_release"] != 1 || r.Counts["missing"] != 1 || len(en.releases(t, "tresor")) != 0 {
			t.Fatalf("a dry run: %+v", r)
		}
		if c := en.cell(t, "tresor"); c.Outcome != "" {
			t.Fatalf("a dry run recorded a cell: %+v", c)
		}
		r := en.sync(t, false)
		if r.Counts[store.CellReleased] != 1 || r.Counts[store.CellMissing] != 1 || r.Error != "" {
			t.Fatalf("the first run: %+v", r)
		}
		rels := en.releases(t, "tresor")
		if len(rels) != 1 || rels[0].Origin != store.OriginUpstream || rels[0].Visibility != store.Private || rels[0].Seq == 0 ||
			!strings.Contains(rels[0].Provenance, `"key":"`+en.fp+`"`) || !strings.Contains(rels[0].Provenance, `"upstream":"acme-repo"`) {
			t.Fatalf("the release: %+v", rels)
		}
		// unchanged: a conditional request
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 0 || en.cell(t, "tresor").Outcome != store.CellReleased {
			t.Fatalf("an unchanged run: %+v", r)
		}
		// a yank survives a run whose ETag changed
		rel := rels[0].Release
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "tresor", rel.ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		en.repo.touch("v2.0.0", "linux_amd64", "tresor")
		if r := en.sync(t, false); r.Counts[store.CellYanked] != 1 {
			t.Fatalf("a yanked release: %+v", r)
		}
		if rs := en.releases(t, "tresor"); len(rs) != 1 || rs[0].State != store.ReleaseYanked {
			t.Fatalf("the yank did not survive: %+v", rs)
		}
		// another body for the same version: a conflict; the version stays
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 2, "tresor_duckdb_cpp_init"))
		if r := en.sync(t, false); r.Counts[store.CellConflict] != 1 {
			t.Fatalf("a conflict: %+v", r)
		}
		// a new version: released and current
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.1", "v2.0.0"), 3, "tresor_duckdb_cpp_init"))
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("a new version: %+v", r)
		}
		rs := en.releases(t, "tresor")
		if len(rs) != 2 || rs[0].ExtVersion != "1.1" || rs[0].Seq <= rel.Seq {
			t.Fatalf("the new version: %+v", rs)
		}
		// cells page by (DuckDB version, platform, name)
		one, err := en.up.Cells(ctx, admin, "acme", "acme-repo", "", [3]string{}, 1)
		if err != nil || len(one) != 1 || one[0].Name != "gone" {
			t.Fatalf("the first page: %+v %v", one, err)
		}
		next, err := en.up.Cells(ctx, admin, "acme", "acme-repo", "", [3]string{one[0].DuckDBVersion, one[0].Platform, one[0].Name}, 10)
		if err != nil || len(next) != 1 || next[0].Name != "tresor" {
			t.Fatalf("the next page: %+v %v", next, err)
		}
		if missing, _ := en.up.Cells(ctx, admin, "acme", "acme-repo", store.CellMissing, [3]string{}, 10); len(missing) != 1 {
			t.Fatalf("by outcome: %+v", missing)
		}
	})
}

func TestIntakeRejects(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		files := map[string][]byte{
			"wrongkey": signed(t, other, cpp("1.0", "v2.0.0"), 1, "wrongkey_duckdb_cpp_init"),
			"unsigned": signed(t, nil, cpp("1.0", "v2.0.0"), 1, "unsigned_duckdb_cpp_init"),
			"footer":   signed(t, en.key, cpp("1.0", "v2.1.0"), 1, "footer_duckdb_cpp_init"),
			"named":    signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "other_duckdb_cpp_init"),
			"filtered": signed(t, en.key, cpp("9.9", "v2.0.0"), 1, "filtered_duckdb_cpp_init"),
		}
		var entries []store.UpstreamEntry
		for n, f := range files {
			en.repo.put("v2.0.0", "linux_amd64", n, f)
			e := store.UpstreamEntry{Name: n}
			if n == "filtered" {
				e.Versions = []string{"1.0"}
			}
			entries = append(entries, e)
		}
		// a gzip bomb: over max_body once inflated
		var bomb bytes.Buffer
		zw := gzip.NewWriter(&bomb)
		_, _ = zw.Write(make([]byte, 4<<20))
		_ = zw.Close()
		en.repo.putRaw("/v2.0.0/linux_amd64/bomb.duckdb_extension.gz", bomb.Bytes())
		// not gzip, not an extension
		en.repo.putRaw("/v2.0.0/linux_amd64/junk.duckdb_extension.gz", []byte("<html>not here</html>"))
		entries = append(entries, store.UpstreamEntry{Name: "bomb"}, store.UpstreamEntry{Name: "junk"})
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(entries...)); err != nil {
			t.Fatal(err)
		}
		r := en.sync(t, false)
		if r.Counts[store.CellRejected] != len(entries) || len(r.Problems) != len(entries) {
			t.Fatalf("rejections: %+v", r)
		}
		for _, n := range []string{"wrongkey", "unsigned", "footer", "named", "filtered", "bomb", "junk"} {
			if len(en.releases(t, n)) != 0 || en.cell(t, n).Outcome != store.CellRejected {
				t.Errorf("%s: %+v", n, en.cell(t, n))
			}
		}
		// a rejected cell is fetched again (no If-None-Match): a fixed file is released
		en.repo.put("v2.0.0", "linux_amd64", "unsigned", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "unsigned_duckdb_cpp_init"))
		en.repo.mu.Lock()
		en.repo.etags["/v2.0.0/linux_amd64/unsigned.duckdb_extension.gz"] = `"same"`
		en.repo.mu.Unlock()
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the fixed cell: %+v", r)
		}
	})
}

func TestShadowsAndBlocks(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		// a replacement in the channel refuses the entry
		repl := signed(t, en.key, cpp("1.0", "v2.0.0"), 9, "tresor_duckdb_cpp_init")
		if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(repl), release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"})); !errors.Is(err, upstream.ErrCollision) {
			t.Fatalf("an entry a replacement holds: %v", err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"})); err != nil {
			t.Fatal(err)
		}
		// a second upstream of the channel cannot list acl
		sp := en.spec(store.UpstreamEntry{Name: "acl"})
		sp.Name = "second"
		if _, err := en.up.Add(ctx, admin, "acme", sp); !errors.Is(err, upstream.ErrCollision) {
			t.Fatalf("a name on two upstreams of a channel: %v", err)
		}
		// a replacement published later wins: the upstream's cell is shadowed
		later := signed(t, en.key, cpp("0.9", "v2.0.0"), 8, "acl_duckdb_cpp_init")
		if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(later), release.AddOptions{Name: "acl"}); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellShadowed] != 1 {
			t.Fatalf("shadowed: %+v", r)
		}
		// a blocked body is not released
		if err := en.up.RemoveEntry(ctx, admin, "acme", "acme-repo", "acl"); err != nil {
			t.Fatal(err)
		}
		f := signed(t, en.key, cpp("1.0", "v2.0.0"), 3, "kwack_duckdb_cpp_init")
		en.repo.put("v2.0.0", "linux_amd64", "kwack", f)
		h, _, _ := extfile.HashBody(bytes.NewReader(f[:len(f)-extfile.SignatureSize]), 1<<30)
		if _, _, err := en.rel.Block(ctx, admin, "acme", h.String(), "CVE"); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "kwack"}); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellBlocked] != 1 {
			t.Fatalf("blocked: %+v", r)
		}
	})
}

// denyReserved allows everything but a reserved name's publication or promotion.
type denyReserved struct{}

func (denyReserved) Allow(_ context.Context, _ authz.Actor, v authz.Verb, r authz.Resource) error {
	if r.Reserved && v != authz.VerbAdmin {
		return authz.ErrDenied
	}
	return nil
}

func TestReservation(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
			t.Fatal(err)
		}
		// a name an upstream of the tenant provides needs a grant naming it, in every channel
		rel := *en.rel
		rel.Authz = denyReserved{}
		file := signed(t, en.key, cpp("2.0", "v2.0.0"), 5, "tresor_duckdb_cpp_init")
		o := release.AddOptions{Name: "tresor", Publish: &release.Publication{Version: "2.0", Platform: "linux_amd64"}}
		if _, _, err := rel.Add(ctx, admin, "acme", "other", bytes.NewReader(file), o); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("publishing an upstream's name: %v", err)
		}
		if _, _, err := rel.Add(ctx, admin, "acme", "other", bytes.NewReader(signed(t, en.key, cpp("2.0", "v2.0.0"), 5, "free_duckdb_cpp_init")),
			release.AddOptions{Name: "free", Publish: &release.Publication{Version: "2.0", Platform: "linux_amd64"}}); err != nil {
			t.Fatalf("publishing another name: %v", err)
		}
	})
}

func TestAddChecks(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		for name, f := range map[string]func(*upstream.Spec){
			"an alias":             func(s *upstream.Spec) { s.Entries = []store.UpstreamEntry{{Name: "postgres"}} },
			"any name in a mirror": func(s *upstream.Spec) { s.Entries = []store.UpstreamEntry{{Name: "*"}} },
			"any name with versions": func(s *upstream.Spec) {
				s.Mode, s.Entries = store.ModePullThrough, []store.UpstreamEntry{{Name: "*", Versions: []string{"1.0"}}}
			},
			"any name, reserved too": func(s *upstream.Spec) {
				s.Mode, s.Entries = store.ModePullThrough, []store.UpstreamEntry{{Name: "*", AllowReserved: true}}
			},
			"any core name": func(s *upstream.Spec) {
				s.Kind, s.Prefix, s.Keys, s.Mode, s.Entries = store.UpstreamCore, "", nil, store.ModePullThrough, []store.UpstreamEntry{{Name: "*"}}
			},
			"over the matrix cap": func(s *upstream.Spec) {
				s.Entries = nil
				for i := range store.MaxUpstreamEntries {
					s.Entries = append(s.Entries, store.UpstreamEntry{Name: fmt.Sprintf("e%04d", i)})
				}
				for i := range 21 {
					s.Platforms = append(s.Platforms, fmt.Sprintf("p%02d_amd64", i))
				}
			},
			"a core name from elsewhere": func(s *upstream.Spec) { s.Entries = []store.UpstreamEntry{{Name: "httpfs"}} },
			"a bad name":                 func(s *upstream.Spec) { s.Entries = []store.UpstreamEntry{{Name: "Bad!"}} },
			"a bad version":              func(s *upstream.Spec) { s.Entries = []store.UpstreamEntry{{Name: "x", Versions: []string{".."}}} },
			"no keys":                    func(s *upstream.Spec) { s.Keys = nil },
			"a bad fingerprint":          func(s *upstream.Spec) { s.Keys = []string{"sha256:XYZ"} },
			"an unlisted key":            func(s *upstream.Spec) { s.Keys = []string{"sha256:" + strings.Repeat("0", 64)} },
			"a query":                    func(s *upstream.Spec) { s.Prefix += "?x=1" },
			"user info":                  func(s *upstream.Spec) { s.Prefix = strings.Replace(s.Prefix, "://", "://u:p@", 1) },
			"dot segments":               func(s *upstream.Spec) { s.Prefix += "/a/../b" },
			"no platforms":               func(s *upstream.Spec) { s.Platforms = nil },
			"wasm":                       func(s *upstream.Spec) { s.Platforms = []string{"wasm_eh"} },
			"a kind":                     func(s *upstream.Spec) { s.Kind = "enterest" },
			"a mode":                     func(s *upstream.Spec) { s.Mode = "push" },
			"pull-through into passthrough": func(s *upstream.Spec) {
				s.Kind, s.Prefix, s.Keys, s.Channel, s.Mode = store.UpstreamCore, "", nil, "mirror", store.ModePullThrough
			},
			"a passthrough channel": func(s *upstream.Spec) { s.Channel = "mirror" },
			"a core prefix":         func(s *upstream.Spec) { s.Kind = store.UpstreamCore },
			"a private address":     func(s *upstream.Spec) { s.Prefix = "https://10.0.0.1" },
		} {
			sp := en.spec(store.UpstreamEntry{Name: "tresor"})
			f(&sp)
			if _, err := en.up.Add(ctx, admin, "acme", sp); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// a core name with allow_reserved, and from duckdb-core
		sp := en.spec(store.UpstreamEntry{Name: "httpfs", AllowReserved: true})
		if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
			t.Fatalf("allow_reserved: %v", err)
		}
		core := upstream.Spec{Name: "core", Kind: store.UpstreamCore, Channel: "other", Platforms: []string{"linux_amd64"},
			Entries: []store.UpstreamEntry{{Name: "json"}}}
		if u, err := en.up.Add(ctx, admin, "acme", core); err != nil || u.Prefix != "" {
			t.Fatalf("a core upstream: %+v %v", u, err)
		}
		if _, err := en.up.Add(ctx, admin, "acme", core); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same name: %v", err)
		}
		// entries replace in place; platforms and keys keep one
		if created, err := en.up.PutEntry(ctx, admin, "acme", "core", store.UpstreamEntry{Name: "json", Versions: []string{"abc"}}); err != nil || created {
			t.Fatalf("replace an entry: %v %v", created, err)
		}
		if err := en.up.RemovePlatform(ctx, admin, "acme", "core", "linux_amd64"); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("the last platform: %v", err)
		}
		if err := en.up.AddKey(ctx, admin, "acme", "core", en.fp); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a key on a core upstream: %v", err)
		}
		u, err := en.up.Get(ctx, admin, "acme", "core")
		if err != nil || len(u.Entries) != 1 || u.Entries[0].Versions[0] != "abc" {
			t.Fatalf("get: %+v %v", u, err)
		}
		if _, err := en.up.Set(ctx, admin, "acme", "core", store.Public, "", u.Version+1); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale version: %v", err)
		}
		if u, err := en.up.Set(ctx, admin, "acme", "core", "", store.UpstreamPaused, u.Version); err != nil || u.State != store.UpstreamPaused {
			t.Fatalf("pause: %+v %v", u, err)
		}
		if _, err := en.up.Sync(ctx, admin, "acme", "core", false); !errors.Is(err, upstream.ErrPaused) {
			t.Fatalf("sync a paused upstream: %v", err)
		}
		if err := en.up.Remove(ctx, admin, "acme", "core", 0); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.Get(ctx, admin, "acme", "core"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("removed: %v", err)
		}
	})
}

func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("%q: %v", s, err)
	}
}

func cellPath(v, p, name string) string {
	return "/" + v + "/" + p + "/" + name + ".duckdb_extension.gz"
}

func (r *fakeRepo) count(path string) (gets, conds int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gets[path], r.conds[path]
}

func TestConditionalRequests(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := cellPath("v2.0.0", "linux_amd64", "tresor")
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "stale"},
			store.UpstreamEntry{Name: "fixme"})); err != nil {
			t.Fatal(err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "fixme", signed(t, nil, cpp("1.0", "v2.0.0"), 1, "fixme_duckdb_cpp_init"))
		r := en.sync(t, false)
		// a 304 to a request that was not conditional is a failure, not a cell
		if r.Counts[store.CellFailed] != 1 || en.cell(t, "stale").Outcome != store.CellFailed {
			t.Fatalf("an unconditional 304: %+v %+v", r, en.cell(t, "stale"))
		}
		if g, c := en.repo.count(p); g != 1 || c != 0 {
			t.Fatalf("the first fetch: %d gets, %d conditional", g, c)
		}
		// unchanged: a conditional request answered 304
		if r := en.sync(t, false); r.Counts[store.CellUnchanged] != 1 {
			t.Fatalf("unchanged: %+v", r)
		}
		if g, c := en.repo.count(p); g != 2 || c != 1 {
			t.Fatalf("the second fetch: %d gets, %d conditional", g, c)
		}
		// a rejected cell is fetched again without If-None-Match: its ETag did not change, the file did
		fp := cellPath("v2.0.0", "linux_amd64", "fixme")
		en.repo.mu.Lock()
		etag := en.repo.etags[fp]
		en.repo.mu.Unlock()
		en.repo.put("v2.0.0", "linux_amd64", "fixme", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "fixme_duckdb_cpp_init"))
		en.repo.mu.Lock()
		en.repo.etags[fp] = etag
		en.repo.mu.Unlock()
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the fixed cell: %+v", r)
		}
		if _, c := en.repo.count(fp); c != 0 {
			t.Fatalf("a rejected cell was fetched conditionally %d times", c)
		}
		// administrators' choices survive a run whose ETag changed
		rel := en.releases(t, "tresor")[0].Release
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "tresor", rel.ID, release.SetPublic, 0); err != nil {
			t.Fatal(err)
		}
		en.repo.touch("v2.0.0", "linux_amd64", "tresor")
		if r := en.sync(t, false); r.Counts[store.CellUnchanged] != 2 || r.Counts[store.CellReleased] != 0 { // tresor and fixme
			t.Fatalf("a changed ETag, the same body: %+v", r)
		}
		if rs := en.releases(t, "tresor"); len(rs) != 1 || rs[0].Visibility != store.Public {
			t.Fatalf("the visibility did not survive: %+v", rs)
		}
		// the release carries the original signature and the provenance
		var n int
		if err := store.QueryRowRaw(ctx, en.st, "SELECT COUNT(*) FROM releases WHERE origin_signature IS NOT NULL AND origin = 'upstream'", &n); err != nil || n != 2 {
			t.Fatalf("original signatures: %d %v", n, err)
		}
		var prov struct{ Upstream, Kind, URL, ETag, Key, FetchedAt string }
		mustJSON(t, strings.NewReplacer(`"fetched_at"`, `"fetchedat"`).Replace(rel.Provenance), &prov)
		if prov.Kind != store.UpstreamRepo || !strings.HasSuffix(prov.URL, p) || prov.ETag == "" || prov.FetchedAt == "" {
			t.Fatalf("provenance: %s", rel.Provenance)
		}
	})
}

func TestFooterRules(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		capiMeta := func(ver, c string) extfile.Metadata {
			return extfile.Metadata{Platform: "linux_amd64", CAPIVersion: c, ExtensionVersion: ver, ABI: extfile.ABICStruct}
		}
		arm := cpp("1.0", "v2.0.0")
		arm.Platform = "linux_arm64"
		unstable := extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.1.0", ExtensionVersion: "1.0", ABI: extfile.ABICStructUnstable}
		en.repo.put("v2.0.0", "linux_amd64", "arm", signed(t, en.key, arm, 1, "arm_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "unst", signed(t, en.key, unstable, 1, "unst_init_c_api_v2"))
		en.repo.put("v2.0.0", "linux_amd64", "newcapi", signed(t, en.key, capiMeta("1.0", "v9.0.0"), 1, "newcapi_init_c_api_v2"))
		var entries []store.UpstreamEntry
		for _, n := range []string{"arm", "unst", "newcapi"} {
			entries = append(entries, store.UpstreamEntry{Name: n})
		}
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(entries...)); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellRejected] != 3 {
			t.Fatalf("footer rules: %+v", r)
		}
	})
}

// A C_STRUCT build from a newer DuckDB version's cell is current for every version it serves.
func TestCStructOrder(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.1.0"}, nil)
			return err
		})
		capiMeta := func(ver string) extfile.Metadata {
			return extfile.Metadata{Platform: "linux_amd64", CAPIVersion: "v1.2.0", ExtensionVersion: ver, ABI: extfile.ABICStruct}
		}
		en.repo.put("v2.0.0", "linux_amd64", "capx", signed(t, en.key, capiMeta("1.0"), 1, "capx_init_c_api"))
		en.repo.put("v2.1.0", "linux_amd64", "capx", signed(t, en.key, capiMeta("1.1"), 2, "capx_init_c_api"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "capx"})); err != nil {
			t.Fatal(err)
		}
		for range 3 { // the order holds whatever the timing
			if r := en.sync(t, false); r.Error != "" {
				t.Fatal(r.Error)
			}
		}
		rs := en.releases(t, "capx")
		if len(rs) != 2 {
			t.Fatalf("releases: %+v", rs)
		}
		var cur store.Candidate
		for _, x := range rs {
			if x.Seq > cur.Seq {
				cur = x
			}
		}
		if cur.ExtVersion != "1.1" {
			t.Fatalf("the current release is %s", cur.ExtVersion)
		}
		// a removed platform's and entry's cells go with the next run
		if err := en.up.RemoveEntry(ctx, admin, "acme", "acme-repo", "capx"); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.PutEntry(ctx, admin, "acme", "acme-repo", store.UpstreamEntry{Name: "other"}); err != nil {
			t.Fatal(err)
		}
		en.sync(t, false)
		if c := en.cell(t, "capx"); c.Outcome != "" {
			t.Fatalf("a removed entry's cell: %+v", c)
		}
	})
}

func TestRunControl(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.1.0"}, nil)
			return err
		})
		p0 := cellPath("v2.0.0", "linux_amd64", "slow")
		en.repo.put("v2.0.0", "linux_amd64", "slow", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "slow_duckdb_cpp_init"))
		en.repo.put("v2.1.0", "linux_amd64", "slow", signed(t, en.key, cpp("1.0", "v2.1.0"), 1, "slow_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "slow"})); err != nil {
			t.Fatal(err)
		}
		// a paused upstream with a pending request does not run
		if _, err := en.up.Sync(ctx, admin, "acme", "acme-repo", false); err != nil {
			t.Fatal(err)
		}
		u, _ := en.up.Get(ctx, admin, "acme", "acme-repo")
		if _, err := en.up.Set(ctx, admin, "acme", "acme-repo", "", store.UpstreamPaused, u.Version); err != nil {
			t.Fatal(err)
		}
		en.run.Once(ctx)
		if g, _ := en.repo.count(p0); g != 0 {
			t.Fatal("a paused upstream ran")
		}
		u, _ = en.up.Get(ctx, admin, "acme", "acme-repo")
		if _, err := en.up.Set(ctx, admin, "acme", "acme-repo", "", store.UpstreamActive, u.Version); err != nil {
			t.Fatal(err)
		}
		// two replicas, one upstream: one run; a request during the run is kept for the next
		gate := make(chan struct{})
		en.repo.mu.Lock()
		en.repo.gate[p0] = gate
		en.repo.mu.Unlock()
		second := &upstream.Runner{Service: en.up, Holder: "second", Log: slog.New(slog.DiscardHandler)}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); en.run.Once(ctx) }()
		<-en.repo.hit
		second.Once(ctx) // the lease is held: nothing runs
		if _, err := en.up.Sync(ctx, admin, "acme", "acme-repo", false); err != nil {
			t.Fatal(err)
		}
		// pausing during the run stops it after the cell in progress: v2.1.0's cell is not fetched
		u, _ = en.up.Get(ctx, admin, "acme", "acme-repo")
		if _, err := en.up.Set(ctx, admin, "acme", "acme-repo", "", store.UpstreamPaused, u.Version); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2100 * time.Millisecond) // the run reads the upstream at most every two seconds
		close(gate)
		wg.Wait()
		if g, _ := en.repo.count(p0); g != 1 {
			t.Fatalf("the slow cell was fetched %d times", g)
		}
		if g, _ := en.repo.count(cellPath("v2.1.0", "linux_amd64", "slow")); g != 0 {
			t.Fatal("a paused run went on")
		}
		u, _ = en.up.Get(ctx, admin, "acme", "acme-repo")
		if u.RequestedAt.IsZero() {
			t.Fatal("the request made during the run was lost")
		}
		var r upstream.Result
		mustJSON(t, u.LastRun, &r)
		if !strings.Contains(r.Error, "paused") {
			t.Fatalf("the stopped run: %+v", r)
		}
	})
}

func gzipReader(b []byte) (io.Reader, error) { return gzip.NewReader(bytes.NewReader(b)) }

func (en *env) releasesIn(t *testing.T, channel, name string) []store.Candidate {
	t.Helper()
	rs, err := en.rel.List(ctx, admin, "acme", channel, name)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}
