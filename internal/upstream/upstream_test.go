package upstream_test

import (
	"bytes"
	"cmp"
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
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	// token, when set, is the Bearer token the files take (a private channel, spec 0009 phase 3):
	// 401 without it, 403 with another; .well-known stays public
	token     string
	auths     []string          // the Authorization headers of file requests
	wellAuth  []string          // the Authorization headers of .well-known requests
	redirects map[string]string // path → where it redirects
	// usage, when set, takes POST <prefix>/-/usage (spec 0012 phase 1c); posts are their bodies
	usage bool
	posts [][]byte
	// busy answers a path 503 that many times first, with Retry-After retryAfter (spec 0032)
	busy       map[string]int
	retryAfter string
	busyStatus int // 503 by default
	// noETag serves a path without an ETag (spec 0038: a cell holding a body, fetched unconditionally)
	noETag map[string]bool
}

// authorizations returns the Authorization headers of file requests so far.
func (r *fakeRepo) authorizations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.auths)
}

func newRepo(t *testing.T) *fakeRepo { return newRepoTLS(t, false) }

func newRepoTLS(t *testing.T, tls bool) *fakeRepo {
	r := &fakeRepo{files: map[string][]byte{}, etags: map[string]string{}, gets: map[string]int{}, conds: map[string]int{},
		gate: map[string]chan struct{}{}, hit: make(chan string, 16), busy: map[string]int{}, retryAfter: "0", noETag: map[string]bool{}}
	h := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
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
			r.wellAuth = append(r.wellAuth, q.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{"signature_keys": r.keys})
			return
		}
		r.auths = append(r.auths, q.Header.Get("Authorization"))
		if to, ok := r.redirects[q.URL.Path]; ok {
			http.Redirect(w, q, to, http.StatusFound)
			return
		}
		if r.token != "" {
			switch q.Header.Get("Authorization") {
			case "Bearer " + r.token:
			case "":
				w.WriteHeader(http.StatusUnauthorized)
				return
			default:
				w.WriteHeader(map[bool]int{true: http.StatusUnauthorized, false: http.StatusForbidden}[strings.HasPrefix(q.Header.Get("Authorization"), "Bearer stale")])
				return
			}
		}
		if q.Method == http.MethodPost {
			if !r.usage || q.URL.Path != "/-/usage" {
				http.NotFound(w, q)
				return
			}
			b, _ := io.ReadAll(q.Body)
			r.posts = append(r.posts, b)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.busy[q.URL.Path] > 0 {
			r.busy[q.URL.Path]--
			if r.retryAfter != "" {
				w.Header().Set("Retry-After", r.retryAfter)
			}
			w.WriteHeader(cmp.Or(r.busyStatus, http.StatusServiceUnavailable))
			return
		}
		b, ok := r.files[q.URL.Path]
		if !ok {
			http.NotFound(w, q)
			return
		}
		etag := r.etags[q.URL.Path]
		if r.noETag[q.URL.Path] {
			_, _ = w.Write(b)
			return
		}
		if q.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write(b)
	})
	if tls {
		r.srv = httptest.NewTLSServer(h)
	} else {
		r.srv = httptest.NewServer(h)
	}
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
	blob    *blob.Service
	blobDir string // the fs store's root
	ks      *keys.Service
	dir     string
	st      *store.Store
	rel     *release.Service
	up      *upstream.Service
	ten     *tenants.Service
	run     *upstream.Runner
	repo    *fakeRepo
	key     *rsa.PrivateKey
	fp      string
}

func newEnv(t *testing.T, e storetest.Engine) *env {
	return newEnvWith(t, e, 1<<20, egress.Config{AllowLoopbackHTTP: true})
}

func newEnvWith(t *testing.T, e storetest.Engine, maxBody int64, ec egress.Config) *env {
	t.Helper()
	st := e.Open(t)
	dir := t.TempDir()
	ks := &keys.Service{Store: st, Signers: signer.Resolver{FileDir: dir, AllowFile: true}, Authz: authz.ServerAdmin{}}
	blobDir := filepath.Join(t.TempDir(), "blobs")
	fsStore, err := fs.Open(blobDir)
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
	en := &env{blob: bs, blobDir: blobDir, ks: ks, dir: dir, st: st, repo: newRepo(t), key: k, fp: extfile.Fingerprint(&k.PublicKey)}
	en.repo.keys = []string{base64.StdEncoding.EncodeToString(der)}
	en.rel = &release.Service{Store: st, Blob: bs, Signers: ks, Authz: authz.ServerAdmin{}}
	en.ten = &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
	// TempDir does not exist: a run creates it, as kista admin bundle export -sync needs
	en.up = &upstream.Service{Store: st, Releases: en.rel, Blob: bs, Fetch: eg, Authz: authz.ServerAdmin{},
		MaxBody: maxBody, MaxIngests: 4, TempDir: filepath.Join(t.TempDir(), "spool", "upstream"), Log: slog.New(slog.DiscardHandler)}
	en.run = &upstream.Runner{Service: en.up, Holder: "test", Log: slog.New(slog.DiscardHandler)}
	must(t, func() error { _, err := en.ten.CreateTenant(ctx, admin, "acme", "", "", ""); return err })
	must(t, func() error {
		_, _, err := en.ten.AddVersion(ctx, admin, "v2.0.0", "release", []string{"v1.5.6", "v2.0.0"}, tenants.CoreExtensions{Names: []string{"json"}})
		return err
	})
	must(t, func() error {
		_, _, err := en.ten.AddVersion(ctx, admin, "v2.1.0", "release", []string{"v1.6.0", "v2.1.0"}, tenants.CoreExtensions{Names: []string{"json"}})
		return err
	})
	for name, kind := range map[string]string{"prod": store.ChannelSigned, "other": store.ChannelSigned, "mirror": store.ChannelPassthrough} {
		must(t, func() error { _, err := en.ten.CreateChannel(ctx, admin, "acme", name, kind, ""); return err })
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
	return signedWith(t, key, m, make([]byte, pad), exports...)
}

// signedWith is signed with extra bytes after the code: a pad, or a licence key block (spec 0021).
func signedWith(t *testing.T, key *rsa.PrivateKey, m extfile.Metadata, extra []byte, exports ...string) []byte {
	t.Helper()
	block, err := extfile.EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	b := append(symtest.ELF(exports...), extra...)
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
		if len(rs) != 2 || rs[0].PackageVersion != "1.1" || rs[0].Seq <= rel.Seq {
			t.Fatalf("the new version: %+v", rs)
		}
		// events (spec 0010): each release made, each run summarised, a cell turning into a refusal
		kinds := map[string]int{}
		evs, _ := en.st.ListEvents(ctx, rs[0].TenantID, store.EventFilter{Limit: 1000})
		for _, e := range evs {
			kinds[e.Kind]++
			if e.Kind == "upstream.release" && e.Actor != "system:upstream:acme/acme-repo" {
				t.Errorf("an upstream release's actor: %s", e.Actor)
			}
		}
		if kinds["upstream.release"] != 2 || kinds["upstream.run"] < 5 || kinds["upstream.rejected"] != 1 {
			t.Fatalf("events: %v", kinds)
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
		// spec 0031: the tenant's release of tresor sets its source; an upstream listing it is added,
		// and its cell is other_source, decided before the download
		repl := signed(t, en.key, cpp("1.0", "v2.0.0"), 9, "tresor_duckdb_cpp_init")
		if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(repl), release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "acl"})); err != nil {
			t.Fatalf("an entry another source holds: %v", err)
		}
		// a second upstream of the channel cannot list acl
		sp := en.spec(store.UpstreamEntry{Name: "acl"})
		sp.Name = "second"
		if _, err := en.up.Add(ctx, admin, "acme", sp); !errors.Is(err, upstream.ErrCollision) {
			t.Fatalf("a name on two upstreams of a channel: %v", err)
		}
		if r := en.sync(t, false); r.Counts[store.CellOtherSource] != 1 || r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		// the upstream's acl holds the name: the tenant's release of it is refused, naming the source
		later := signed(t, en.key, cpp("0.9", "v2.0.0"), 8, "acl_duckdb_cpp_init")
		_, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(later), release.AddOptions{Name: "acl"})
		if !errors.Is(err, release.ErrOtherSource) || !strings.Contains(err.Error(), "upstream:acme-repo") {
			t.Fatalf("a release under the upstream's name: %v", err)
		}
		// switched to the tenant: its release lands, and the upstream's cell is other_source
		if _, err := en.rel.SetSource(ctx, admin, "acme", "prod", "acl", "tenant", ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(later), release.AddOptions{Name: "acl"}); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellOtherSource] != 2 {
			t.Fatalf("other_source: %+v", r)
		}
		// switched back: the upstream's release is served again, and its cell unchanged
		if _, err := en.rel.SetSource(ctx, admin, "acme", "prod", "acl", "upstream:acme-repo", ""); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellOtherSource] != 1 || r.Counts[store.CellUnchanged] != 1 {
			t.Fatalf("switched back: %+v", r)
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
		if cur.PackageVersion != "1.1" {
			t.Fatalf("the current release is %s", cur.PackageVersion)
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

// Spec 0014: a bundle-mode upstream never contacts its source: its prefix is an identity (no
// egress port check, no .well-known fetch, https only), it takes no credential, it is never run
// or made due, and a passthrough channel takes a core one.
func TestBundleMode(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnvWith(t, e, 1<<20, egress.Config{}) // no loopback: egress's port rule applies
			en.up.Config.Interval = time.Hour              // scheduling on
			sp := en.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Name, sp.Prefix = "air", "https://air.example.org:9443/acme/prod" // a port egress does not reach
			sp.Keys = []string{"sha256:" + strings.Repeat("1", 64)}              // listed nowhere: checked at import
			if _, err := en.up.Add(ctx, admin, "acme", sp); err == nil || !strings.Contains(err.Error(), "egress does not reach that port") {
				t.Fatalf("the same prefix in mirror mode: %v", err)
			}
			sp.Mode = store.ModeBundle
			u, err := en.up.Add(ctx, admin, "acme", sp)
			if err != nil || u.Mode != store.ModeBundle || u.Prefix != "https://air.example.org:9443/acme/prod" {
				t.Fatalf("a bundle-mode repository upstream: %+v %v", u, err)
			}
			if g, _ := en.repo.count("/.well-known/duckdb-extension-repo.json"); g != 0 {
				t.Errorf("a source was contacted: %d", g)
			}
			http := sp
			http.Name, http.Prefix = "air-http", "http://air.example.org/acme/prod"
			if _, err := en.up.Add(ctx, admin, "acme", http); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("an http prefix: %v", err)
			}
			for _, dry := range []bool{false, true} {
				if _, err := en.up.Sync(ctx, admin, "acme", "air", dry); !errors.Is(err, upstream.ErrBundle) {
					t.Errorf("sync (dry %v): %v", dry, err)
				}
			}
			if _, err := en.up.SetCredential(ctx, admin, "acme", "air", "any", u.Version); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("a credential on the route: %v", err)
			}
			cred := sp
			cred.Name, cred.Credential = "air-cred", "any"
			if _, err := en.up.Add(ctx, admin, "acme", cred); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("a credential at creation: %v", err)
			}
			// everything that makes a mirror due leaves it alone: pause and resume, a platform, an
			// entry, a DuckDB version of its channel
			u, _ = en.up.Get(ctx, admin, "acme", "air")
			if u, err = en.up.Set(ctx, admin, "acme", "air", "", store.UpstreamPaused, u.Version); err != nil {
				t.Fatal(err)
			}
			if _, err := en.up.Sync(ctx, admin, "acme", "air", false); !errors.Is(err, upstream.ErrBundle) {
				t.Errorf("sync while paused: %v (the mode answers first)", err)
			}
			if _, err := en.up.Set(ctx, admin, "acme", "air", "", store.UpstreamActive, u.Version); err != nil {
				t.Fatal(err)
			}
			must(t, func() error { return en.up.AddPlatform(ctx, admin, "acme", "air", "linux_arm64") })
			must(t, func() error {
				_, err := en.up.PutEntry(ctx, admin, "acme", "air", store.UpstreamEntry{Name: "other"})
				return err
			})
			must(t, func() error {
				_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.1.0"}, nil)
				return err
			})
			en.run.Once(ctx)
			if u, _ = en.up.Get(ctx, admin, "acme", "air"); !u.LastRunAt.IsZero() || !u.NextRunAt.IsZero() || !u.RequestedAt.IsZero() {
				t.Errorf("a bundle-mode upstream was scheduled or run: %+v", u)
			}
			core := upstream.Spec{Name: "air-core", Kind: store.UpstreamCore, Channel: "mirror", Mode: store.ModeBundle,
				Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "json"}}}
			if u, err := en.up.Add(ctx, admin, "acme", core); err != nil || u.Visibility != store.Public {
				t.Fatalf("a core bundle-mode upstream into a passthrough channel: %+v %v", u, err)
			}
		})
	}
}

// Spec 0014: a plan merges a tenant's bundle-mode upstreams by source, unions their keys, cells
// and version filters, leaves development builds out of DuckDB's sources, and states what the site
// holds only where every sharing upstream agrees.
func TestPlan(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		must(t, func() error {
			_, _, err := en.ten.AddVersion(ctx, admin, "eb0d9df1a2", "dev", nil, tenants.CoreExtensions{Names: []string{"json"}})
			return err
		})
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"eb0d9df1a2"}, nil)
			return err
		})
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "mirror", []string{"v2.0.0"}, nil)
			return err
		})
		fp := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
		const prefix = "https://src.example.org/acme/prod"
		add := func(sp upstream.Spec) store.Upstream {
			t.Helper()
			sp.Mode = store.ModeBundle
			if sp.Platforms == nil {
				sp.Platforms = []string{"linux_amd64"}
			}
			u, err := en.up.Add(ctx, admin, "acme", sp)
			if err != nil {
				t.Fatalf("%s: %v", sp.Name, err)
			}
			return u
		}
		coreA := add(upstream.Spec{Name: "core-a", Kind: store.UpstreamCore, Channel: "mirror", Entries: []store.UpstreamEntry{{Name: "json"}}})
		coreB := add(upstream.Spec{Name: "core-b", Kind: store.UpstreamCore, Channel: "prod",
			Entries: []store.UpstreamEntry{{Name: "json", Versions: []string{"1.0.0"}}}})
		repoA := add(upstream.Spec{Name: "repo-a", Kind: store.UpstreamRepo, Prefix: prefix, Channel: "prod", Keys: []string{fp("1")},
			Entries: []store.UpstreamEntry{{Name: "x", Versions: []string{"1.0"}}}})
		repoB := add(upstream.Spec{Name: "repo-b", Kind: store.UpstreamRepo, Prefix: prefix + "/", Channel: "other", Keys: []string{fp("2")},
			Entries: []store.UpstreamEntry{{Name: "x"}}})
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"})); err != nil { // a mirror: not planned
			t.Fatal(err)
		}
		h := func(c string) string { return strings.Repeat(c, 64) }
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				for _, c := range []store.UpstreamCell{
					{UpstreamID: coreA.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "json", ETag: `"e"`, BodyHash: h("a"), Outcome: store.CellReleased},
					{UpstreamID: coreB.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "json", ETag: `"e"`, BodyHash: h("a"), Outcome: store.CellUnchanged},
					{UpstreamID: repoA.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "x", ETag: `"1"`, BodyHash: h("b"), Outcome: store.CellReleased},
					{UpstreamID: repoB.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "x", ETag: `"2"`, BodyHash: h("c"), Outcome: store.CellReleased},
					{UpstreamID: repoA.ID, DuckDBVersion: "eb0d9df1a2", Platform: "linux_amd64", Name: "x", Outcome: store.CellMissing},
				} {
					c.FetchedAt = time.Now()
					if err := tx.PutCell(ctx, c); err != nil {
						return err
					}
				}
				return nil
			})
		})
		if _, err := en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{}, nil); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("no bundle key: %v", err)
		}
		if _, err := en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{Upstreams: []string{"acme-repo"}}, []string{fp("9")}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("a mirror named: %v", err)
		}
		p, err := en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{}, []string{fp("9")})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Sources) != 2 || p.Sources[0].Kind != store.UpstreamCore || p.Sources[1].Prefix != prefix {
			t.Fatalf("sources: %+v", p.Sources)
		}
		core := p.Sources[0].Cells
		if len(core) != 1 || core[0].DuckDBVersion != "v2.0.0" || len(core[0].Versions) != 0 || core[0].BodyHash != h("a") || core[0].ETag != `"e"` {
			t.Errorf("core cells (no dev build, any version, held): %+v", core)
		}
		repo := p.Sources[1]
		if len(repo.Keys) != 2 || len(repo.Cells) != 2 {
			t.Fatalf("repository: %+v", repo)
		}
		for _, c := range repo.Cells {
			switch c.DuckDBVersion {
			case "v2.0.0": // two upstreams disagree on what they hold; one takes any version
				if c.BodyHash != "" || c.ETag != "" || len(c.Versions) != 0 {
					t.Errorf("v2.0.0: %+v", c)
				}
			case "eb0d9df1a2": // another kista may serve a development build; only repo-a asks, with its filter
				if c.BodyHash != "" || len(c.Versions) != 1 || c.Versions[0] != "1.0" {
					t.Errorf("eb0d9df1a2: %+v", c)
				}
			default:
				t.Errorf("a cell %+v", c)
			}
		}
		if p.Tenant != "acme" || p.Format != bundle.PlanFormat || p.BundleKeys[0] != fp("9") {
			t.Errorf("the plan: %+v", p)
		}
		one, err := en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{Upstreams: []string{"core-a"}}, []string{fp("9")})
		if err != nil || len(one.Sources) != 1 || one.Sources[0].Kind != store.UpstreamCore {
			t.Errorf("one upstream: %+v %v", one, err)
		}
		if _, err := en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{Upstreams: []string{"nope"}}, []string{fp("9")}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an unknown upstream: %v", err)
		}
		// a paused upstream is left out, and refused when named; core-a alone then holds json, and
		// core-b's cell no longer spoils the agreement
		u, _ := en.up.Get(ctx, admin, "acme", "core-b")
		if _, err := en.up.Set(ctx, admin, "acme", "core-b", "", store.UpstreamPaused, u.Version); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{Upstreams: []string{"core-b"}}, []string{fp("9")}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("a paused upstream named: %v", err)
		}
		p, err = en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{}, []string{fp("9")})
		if err != nil || len(p.Sources[0].Cells) != 1 || p.Sources[0].Cells[0].BodyHash != h("a") {
			t.Errorf("paused left out: %+v %v", p.Sources, err)
		}
	})
}

// Spec 0014 phase 1b: a plan's feeds are the active bundle-mode feeds, merged by URL: their keys
// and kinds united (one accepting every kind makes it every kind), the bodies offered with the
// lowest cursor among them, and the plan's sources each one's offer takes; -feed and -upstream
// alone leave the other kind out.
func TestPlanFeeds(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		fp := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
		file := signed(t, en.key, cpp("1.0", "v2.0.0"), 9, "tresor_duckdb_cpp_init")
		if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(file), release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		ef, err := extfile.Open(bytes.NewReader(file), int64(len(file)), 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		body := ef.Hash.String()
		up, err := en.up.Add(ctx, admin, "acme", upstream.Spec{Name: "air", Kind: store.UpstreamCore, Channel: "mirror", Mode: store.ModeBundle,
			Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "json"}}})
		if err != nil {
			t.Fatal(err)
		}
		tn, _ := en.st.GetTenant(ctx, "acme")
		prod, _ := en.st.GetChannel(ctx, "acme", "prod")
		const url = "https://src.example.org/api/v1/tenants/reviews"
		feed := func(name string, f store.Feed) store.Feed {
			t.Helper()
			f.TenantID, f.Name, f.URL, f.CreatedBy = tn.ID, name, url, "os:1:t"
			if f.Mode == "" {
				f.Mode = store.FeedBundle
			}
			if f.Offer == "" {
				f.Offer = store.OfferUpstreams
			}
			must(t, func() error { return en.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertFeed(ctx, &f) }) })
			return f
		}
		a := feed("a", store.Feed{Keys: []string{fp("1")}, Kinds: []string{store.KindReview}, Offer: store.OfferChannels, Channels: []string{prod.ID}})
		b := feed("b", store.Feed{Keys: []string{fp("2")}, Offer: store.OfferChannels, Channels: []string{prod.ID}})
		feed("online", store.Feed{Keys: []string{fp("3")}, Mode: store.FeedOnline, Credential: "src"}) // never planned
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				if err := tx.SetFeedCursor(ctx, a.ID, body, 7); err != nil {
					return err
				}
				return tx.SetFeedCursor(ctx, b.ID, body, 4)
			})
		})
		p, err := en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{}, []string{fp("9")})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Sources) != 1 || len(p.Feeds) != 1 {
			t.Fatalf("plan: %+v", p)
		}
		f := p.Feeds[0]
		if f.URL != url || len(f.Keys) != 2 || len(f.Kinds) != 0 || len(f.Bodies) != 1 || f.Bodies[0].BodyHash != body || f.Bodies[0].Seq != 4 ||
			len(f.Sources) != 0 {
			t.Fatalf("the feed: %+v", f) // the channels offer takes prod; the bundle-mode upstream is into mirror
		}
		// an upstreams offer takes the bundle-mode upstreams' sources; the tenant's own release is not offered
		feed("c", store.Feed{Keys: []string{fp("4")}})
		p, err = en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{Feeds: []string{"c"}}, []string{fp("9")})
		if err != nil || len(p.Sources) != 0 || len(p.Feeds) != 1 || len(p.Feeds[0].Bodies) != 0 {
			t.Fatalf("-feed alone: %+v %v", p, err)
		}
		p, err = en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{Upstreams: []string{up.Name}}, []string{fp("9")})
		if err != nil || len(p.Sources) != 1 || len(p.Feeds) != 0 {
			t.Fatalf("-upstream alone: %+v %v", p, err)
		}
		p, err = en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{Upstreams: []string{up.Name}, Feeds: []string{"c"}}, []string{fp("9")})
		if err != nil || len(p.Feeds) != 1 || len(p.Feeds[0].Sources) != 1 || p.Feeds[0].Sources[0] != 0 {
			t.Fatalf("both: %+v %v", p, err)
		}
		feed("paused", store.Feed{Keys: []string{fp("5")}, State: store.FeedPaused})
		for _, name := range []string{"online", "nope", "paused"} {
			if _, err := en.up.Plan(ctx, admin, "acme", upstream.PlanTargets{Feeds: []string{name}}, []string{fp("9")}); err == nil {
				t.Errorf("%s named", name)
			}
		}
	})
}

// Spec 0024: CheckAddAll makes Add's checks for several upstreams, each as if those before it were
// added, and writes nothing; it names the first one refused. Add refuses what it refuses.
func TestCheckAddAll(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		tn, _ := en.st.GetTenant(ctx, "acme")
		events := func() int {
			evs, _ := en.st.ListEvents(ctx, tn.ID, store.EventFilter{Limit: 1000})
			return len(evs)
		}
		before := events()
		a := en.spec(store.UpstreamEntry{Name: "tresor"})
		b := upstream.Spec{Name: "core", Kind: store.UpstreamCore, Channel: "other", Platforms: []string{"linux_amd64"},
			Entries: []store.UpstreamEntry{{Name: "json"}}}
		if i, err := en.up.CheckAddAll(ctx, admin, "acme", []upstream.Spec{a, b}); err != nil || i != -1 {
			t.Fatalf("both: %d %v", i, err)
		}
		if ups, _ := en.up.List(ctx, admin, "acme"); len(ups) != 0 || events() != before {
			t.Fatalf("written: %d upstreams, %d events", len(ups), events()-before)
		}
		// two that collide with each other: the second is named
		c := b
		c.Name = "core2"
		i, err := en.up.CheckAddAll(ctx, admin, "acme", []upstream.Spec{a, b, c})
		if !errors.Is(err, upstream.ErrCollision) || i != 2 {
			t.Fatalf("a collision among them: %d %v", i, err)
		}
		// Add takes the first two, and refuses the third as CheckAddAll did
		for _, sp := range []upstream.Spec{a, b} {
			if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := en.up.Add(ctx, admin, "acme", c); !errors.Is(err, upstream.ErrCollision) {
			t.Fatalf("add: %v", err)
		}
		// a field refused names its upstream before anything is read of the store
		bad := a
		bad.Name, bad.Keys = "bad", []string{"sha256:XYZ"}
		if i, err := en.up.CheckAddAll(ctx, admin, "acme", []upstream.Spec{b, bad}); !errors.Is(err, store.ErrInvalid) || i != 1 {
			t.Fatalf("a bad field: %d %v", i, err)
		}
	})
}

// Spec 0031: an upstream removed and added again (a new id) succeeds the removed one: its names and
// its releases of them in the channel are the new upstream's (spec 0033: moved to the new source's
// slots), so a version the administrator yanked stays yanked.
func TestUpstreamSuccession(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"})); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		ch, err := en.st.GetChannel(ctx, "acme", "prod")
		if err != nil {
			t.Fatal(err)
		}
		first, err := en.st.ListReleases(ctx, ch.ID, "acl")
		if err != nil || len(first) != 1 {
			t.Fatalf("the first release: %+v %v", first, err)
		}
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "acl", first[0].ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		if err := en.up.Remove(ctx, admin, "acme", "acme-repo", 0); err != nil {
			t.Fatal(err)
		}
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"}))
		if err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellYanked] != 1 {
			t.Fatalf("the run of the new upstream: %+v", r)
		}
		if sel, err := en.st.ChannelSources(ctx, ch.ID); err != nil || sel["acl"].Source != store.UpstreamSource(u.ID) {
			t.Fatalf("the source: %+v %v", sel, err)
		}
		rs, err := en.st.ListReleases(ctx, ch.ID, "acl")
		if err != nil || len(rs) != 1 || rs[0].ID != first[0].ID || rs[0].Source != store.UpstreamSource(u.ID) || rs[0].State != store.ReleaseYanked {
			t.Fatalf("the release's source: %+v %v", rs, err)
		}
	})
}

// Spec 0033: under a manual rule, a mirror's new version lands in the catalogue (the cell's outcome
// landed), is placed nowhere and kept from the collector, and is placed by hand.
func TestUpstreamManualLands(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"})); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		v, err := en.rel.Source(ctx, admin, "acme", "prod", "acl")
		if err != nil || v.Mode != store.RuleNone {
			t.Fatalf("the rule the mirror's arrival wrote: %+v %v", v, err)
		}
		if _, err := en.rel.SetSource(ctx, admin, "acme", "prod", "acl", v.Source, store.RuleManual); err != nil {
			t.Fatal(err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("2.0", "v2.0.0"), 2, "acl_duckdb_cpp_init"))
		if r := en.sync(t, false); r.Counts[store.CellLanded] != 1 {
			t.Fatalf("the run under a manual rule: %+v", r)
		}
		ch, err := en.st.GetChannel(ctx, "acme", "prod")
		if err != nil {
			t.Fatal(err)
		}
		if rs, err := en.st.ListReleases(ctx, ch.ID, "acl"); err != nil || len(rs) != 1 || rs[0].PackageVersion != "1.0" {
			t.Fatalf("placed under a manual rule: %+v %v", rs, err)
		}
		dead, err := en.st.DeadBuilds(ctx, "default", time.Now().Add(time.Hour), "", 100)
		if err != nil || len(dead) != 0 {
			t.Fatalf("a landed build collectable: %v %v", dead, err)
		}
		rs, _, err := en.rel.Place(ctx, admin, "acme", "prod", "acl", release.PlaceOptions{Source: v.Source, Version: "2.0"})
		// a mirror's build placed stays its mirror's: origin upstream, its original signature
		if err != nil || len(rs) != 1 || rs[0].Origin != store.OriginUpstream || len(rs[0].OriginSignature) != 256 ||
			!strings.Contains(rs[0].Provenance, `"placed":"hand"`) {
			t.Fatalf("placed by hand: %+v %v", rs, err)
		}
		if r := en.sync(t, false); r.Counts[store.CellUnchanged] != 1 {
			t.Fatalf("the run after: %+v", r)
		}
		// a landed build its cell no longer holds (the cell moved on) is left to the collector
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("3.0", "v2.0.0"), 3, "acl_duckdb_cpp_init"))
		en.sync(t, false)
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("4.0", "v2.0.0"), 4, "acl_duckdb_cpp_init"))
		if r := en.sync(t, false); r.Counts[store.CellLanded] != 1 {
			t.Fatalf("4.0: %+v", r)
		}
		if n, err := en.rel.TrimLanded(ctx); err != nil || n != 1 {
			t.Fatalf("trimmed: %d %v", n, err)
		}
	})
}

// Spec 0033: an upstream added again that brings another body of a version the removed one released
// in the channel takes the removed one's release, and its cell is a conflict (the version is
// immutable within its source), not a failure every run.
func TestUpstreamSuccessionOtherBody(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "acl_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"})); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		if err := en.up.Remove(ctx, admin, "acme", "acme-repo", 0); err != nil {
			t.Fatal(err)
		}
		en.repo.put("v2.0.0", "linux_amd64", "acl", signed(t, en.key, cpp("1.0", "v2.0.0"), 9, "acl_duckdb_cpp_init", "other"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "acl"})); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellConflict] != 1 {
			t.Fatalf("the new upstream's run: %+v", r)
		}
		// the next run asks conditionally, as for any conflict: nothing is rejected or released
		if r := en.sync(t, false); r.Counts[store.CellRejected] != 0 || r.Counts[store.CellReleased] != 0 {
			t.Fatalf("the run again: %+v", r)
		}
	})
}
