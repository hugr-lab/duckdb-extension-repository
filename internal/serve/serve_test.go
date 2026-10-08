package serve

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

var (
	ctx   = context.Background()
	admin = authz.Actor{Kind: authz.ActorOS, ID: "1:test"}
)

type env struct {
	st      *store.Store
	keys    *keys.Service
	rel     *release.Service
	h       *Handler
	srv     *httptest.Server
	fsStore *fs.Store
	files   map[string][]byte // release id -> the file added
	pub     *rsa.PublicKey
}

func ext(t *testing.T, n int, seed uint64, m extfile.Metadata) []byte {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	block, err := extfile.EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, extfile.MetadataPrefix...)
	b = append(b, block[:]...)
	return append(b, make([]byte, extfile.SignatureSize)...)
}

func cpp(ver string) extfile.Metadata {
	return extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: ver, ABI: extfile.ABICPP}
}

func newEnv(t *testing.T, o Options, scheme string) *env {
	t.Helper()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ks := &keys.Service{Store: st, Signers: signer.Resolver{FileDir: dir, AllowFile: true}, Authz: authz.ServerAdmin{}}
	fsStore, err := fs.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: fsStore}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 64 << 20, MaxIngests: 4, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bs.Close() })
	ten := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
	mustDo(t, func() error { _, err := ten.CreateTenant(ctx, admin, "acme", "", ""); return err })
	mustDo(t, func() error {
		_, err := ten.AddVersion(ctx, admin, "v2.0.0", "release", []string{"v1.5.6", "v2.0.0"})
		return err
	})
	for name, kind := range map[string]string{"prod": store.ChannelSigned, "mirror": store.ChannelPassthrough, "empty": store.ChannelSigned} {
		mustDo(t, func() error { _, err := ten.CreateChannel(ctx, admin, "acme", name, kind); return err })
	}
	mustDo(t, func() error {
		_, err := ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.0.0"}, nil)
		return err
	})
	if err := signer.GenerateKeyFile(filepath.Join(dir, "a.pem")); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Add(ctx, admin, "acme", "prod", "file:a.pem", true)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := x509.ParsePKIXPublicKey(k.PublicKey)
	if o.MaxDownloads == 0 {
		o = Options{MaxDownloads: 8, MaxDownloadsPerClient: 4, MinRate: 1024, WriteIdleTimeout: 10 * time.Second}
	}
	o.Log = slog.New(slog.DiscardHandler)
	en := &env{st: st, keys: ks, fsStore: fsStore, files: map[string][]byte{}, pub: pub.(*rsa.PublicKey),
		rel: &release.Service{Store: st, Blob: bs, Signers: ks, Authz: authz.ServerAdmin{}}}
	en.h = NewHandler(st, ks, bs, o)
	en.h.SetReady(true)
	en.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		en.h.ServeHTTP(w, r.WithContext(withScheme(r.Context(), scheme)))
	}))
	en.srv.Config.DisableGeneralOptionsHandler = true // as Server.build does
	en.srv.Start()
	t.Cleanup(en.srv.Close)
	return en
}

func mustDo(t *testing.T, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Fatal(err)
	}
}

func (en *env) add(t *testing.T, file []byte, o release.AddOptions) store.Release {
	t.Helper()
	r, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(file), o)
	if err != nil {
		t.Fatal(err)
	}
	en.files[r.ID] = file
	return r
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

func (en *env) do(t *testing.T, method, path string, hdr map[string]string) answer {
	t.Helper()
	req, err := http.NewRequest(method, en.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading: %v", method, path, err)
	}
	resp.Header.Del("Date")
	return answer{resp.StatusCode, resp.Header, b}
}

// rawDo sends a request line as written (no client-side cleaning).
func (en *env) rawDo(t *testing.T, line string) int {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(en.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, line+"\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufioReader(c), nil)
	if err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

const flat = "/acme/prod/v2.0.0/linux_amd64/"

func (en *env) checkFile(t *testing.T, body []byte, gz bool, r store.Release) {
	t.Helper()
	if gz {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	file := en.files[r.ID]
	n := len(file) - extfile.SignatureSize
	if !bytes.Equal(body[:n], file[:n]) {
		t.Fatal("the body differs")
	}
	f, err := extfile.Open(bytes.NewReader(body), int64(len(body)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := extfile.Verify(f.Hash, f.Signature, []*rsa.PublicKey{en.pub}); !ok {
		t.Fatal("the served signature does not verify with the channel key")
	}
}

func TestServe(t *testing.T) {
	en := newEnv(t, Options{}, "https")
	r10 := en.add(t, ext(t, 3<<20, 1, cpp("1.0")), release.AddOptions{Name: "tresor"})
	r11 := en.add(t, ext(t, 4000, 2, cpp("1.1")), release.AddOptions{Name: "tresor", Private: true})
	demo := en.add(t, ext(t, 5000, 3, extfile.Metadata{Platform: "linux_amd64", CAPIVersion: "v1.2.0", ExtensionVersion: "0.1", ABI: extfile.ABICStruct}),
		release.AddOptions{Name: "demo"})

	t.Run("gz, plain, head, ranges, etags", func(t *testing.T) {
		a := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", nil)
		if a.status != 200 || a.header.Get("Content-Type") != "application/gzip" ||
			a.header.Get("Cache-Control") != "public, no-cache, no-transform" || a.header.Get("Vary") != "Authorization" ||
			a.header.Get("X-Content-Type-Options") != "nosniff" || a.header.Get("ETag") == "" {
			t.Fatalf("gz: %d %v", a.status, a.header)
		}
		en.checkFile(t, a.body, true, r10) // the newer private release is not current for this caller
		etag := a.header.Get("ETag")
		if h := en.do(t, "HEAD", flat+"tresor.duckdb_extension.gz", nil); h.status != 200 || len(h.body) != 0 ||
			h.header.Get("Content-Length") != itoa(len(a.body)) {
			t.Fatalf("head: %d %v", h.status, h.header)
		}
		if p := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", map[string]string{"Range": "bytes=100-199"}); p.status != 206 || !bytes.Equal(p.body, a.body[100:200]) {
			t.Fatalf("range: %d", p.status)
		}
		if p := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", map[string]string{"Range": "bytes=0-9,20-29"}); p.status != 200 || !bytes.Equal(p.body, a.body) {
			t.Fatalf("multiple ranges: %d", p.status)
		}
		if p := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", map[string]string{"Range": "bytes=99999999-"}); p.status != 416 ||
			p.header.Get("Cache-Control") != "private, no-store" || p.header.Get("ETag") != "" {
			t.Fatalf("unsatisfiable range: %d %v", p.status, p.header)
		}
		if p := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", map[string]string{"If-None-Match": etag}); p.status != 304 || p.header.Get("ETag") != etag {
			t.Fatalf("if-none-match: %d %v", p.status, p.header)
		}
		pl := en.do(t, "GET", flat+"tresor.duckdb_extension", nil)
		if pl.status != 200 || pl.header.Get("Accept-Ranges") != "" || pl.header.Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("plain: %d %v", pl.status, pl.header)
		}
		en.checkFile(t, pl.body, false, r10)
		if p := en.do(t, "GET", flat+"tresor.duckdb_extension", map[string]string{"If-None-Match": pl.header.Get("ETag")}); p.status != 304 {
			t.Fatalf("plain 304: %d", p.status)
		}
		if p := en.do(t, "GET", flat+"tresor.duckdb_extension", map[string]string{"Range": "bytes=0-9"}); p.status != 200 || len(p.body) != len(pl.body) {
			t.Fatalf("plain with a range is sent whole: %d", p.status)
		}
		// versioned path, and a c_struct build under a channel version that accepts it
		v := en.do(t, "GET", "/acme/prod/tresor/1.0/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", nil)
		if v.status != 200 || !bytes.Equal(v.body, a.body) {
			t.Fatalf("versioned: %d", v.status)
		}
		d := en.do(t, "GET", flat+"demo.duckdb_extension.gz", nil)
		if d.status != 200 {
			t.Fatalf("c_struct: %d", d.status)
		}
		en.checkFile(t, d.body, true, demo)
	})

	t.Run("missing answers are identical", func(t *testing.T) {
		paths := []string{
			"/acme/prod/tresor/1.1/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", // private
			"/acme/prod/tresor/1.1/v2.0.0/linux_amd64/tresor.duckdb_extension",
			"/acme/prod/tresor/9.9/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", // never existed
			flat + "nothing.duckdb_extension.gz",
			"/acme/prod/v2.1.0/linux_amd64/tresor.duckdb_extension.gz", // a version the channel does not serve
			"/acme/prod/v2.0.0/osx_arm64/tresor.duckdb_extension.gz",
		}
		var first answer
		for i, p := range paths {
			a := en.do(t, "GET", p, nil)
			if a.status != 401 || a.header.Get("WWW-Authenticate") != `Bearer realm="kista"` ||
				a.header.Get("Cache-Control") != "private, no-store" || a.header.Get("ETag") != "" {
				t.Fatalf("%s: %d %v", p, a.status, a.header)
			}
			if i == 0 {
				first = a
			} else if !bytes.Equal(a.body, first.body) || !sameHeaders(a.header, first.header) {
				t.Fatalf("%s answers differently from %s", p, paths[0])
			}
			// conditional and range headers change nothing before the decision
			b := en.do(t, "GET", p, map[string]string{"Range": "bytes=0-1", "If-None-Match": "*"})
			if b.status != 401 || !bytes.Equal(b.body, first.body) {
				t.Fatalf("%s with conditions: %d", p, b.status)
			}
		}
		_ = r11
	})

	t.Run("not found, refused paths, methods", func(t *testing.T) {
		for _, p := range []string{"/other/prod" + flat[10:] + "tresor.duckdb_extension.gz", "/acme/nope/v2.0.0/linux_amd64/tresor.duckdb_extension.gz",
			"/acme/mirror/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", "/acme/mirror/.well-known/duckdb-extension-repo.json",
			"/acme/empty/.well-known/duckdb-extension-repo.json", flat + "tresor.duckdb_extension.gz?x=1", flat + "tresor.duckdb_extension.gz/",
			flat + "tresor.zip", "/acme/prod/tresor/1.0/v2.0.0/linux_amd64/demo.duckdb_extension.gz", "/acme/prod/v2.0.0/wasm_eh/tresor.duckdb_extension.gz",
			"/", "/acme", "/acme/prod/.well-known/other.json"} {
			if a := en.do(t, "GET", p, nil); a.status != 404 {
				t.Errorf("%s: %d", p, a.status)
			}
		}
		for _, line := range []string{"GET /acme/prod/v2.0.0/linux_amd64/./tresor.duckdb_extension.gz HTTP/1.1",
			"GET /acme/prod/v2.0.0/linux_amd64/%74resor.duckdb_extension.gz HTTP/1.1",
			"GET /acme//prod/v2.0.0/linux_amd64/tresor.duckdb_extension.gz HTTP/1.1",
			"GET /acme/prod/v2.0.0/linux_amd64/../linux_amd64/tresor.duckdb_extension.gz HTTP/1.1",
			"GET http://x/acme/prod/v2.0.0/linux_amd64/tresor.duckdb_extension.gz HTTP/1.1",
			"GET * HTTP/1.1", "OPTIONS * HTTP/1.1"} {
			if s := en.rawDo(t, line); s != 404 {
				t.Errorf("%q: %d", line, s)
			}
		}
		if a := en.do(t, "POST", flat+"tresor.duckdb_extension.gz", nil); a.status != 405 || a.header.Get("Allow") != "GET, HEAD" {
			t.Fatalf("post: %d", a.status)
		}
		if a := en.do(t, "OPTIONS", flat+"tresor.duckdb_extension.gz", nil); a.status != 405 {
			t.Fatalf("options: %d", a.status)
		}
		req, _ := http.NewRequest("GET", en.srv.URL+flat+"tresor.duckdb_extension.gz", strings.NewReader("body"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("a body: %d", resp.StatusCode)
		}
	})

	t.Run("404 answers are identical", func(t *testing.T) {
		var first answer
		for i, p := range []string{"/other/prod/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", "/acme/nope/.well-known/duckdb-extension-repo.json",
			"/acme/mirror/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", flat + "tresor.zip"} {
			a := en.do(t, "GET", p, nil)
			if a.status != 404 || a.header.Get("Cache-Control") != "private, no-store" || a.header.Get("Vary") != "Authorization" {
				t.Fatalf("%s: %d %v", p, a.status, a.header)
			}
			if i == 0 {
				first = a
			} else if !bytes.Equal(a.body, first.body) || !sameHeaders(a.header, first.header) {
				t.Fatalf("%s answers differently", p)
			}
		}
	})

	t.Run("no write preconditions", func(t *testing.T) {
		if a := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", map[string]string{"If-Match": `"nope"`}); a.status != 200 {
			t.Fatalf("If-Match: %d", a.status)
		}
	})

	t.Run("a public or unconfigured storage domain is refused after the decision", func(t *testing.T) {
		en.h.SetPublicDomains([]string{"default"})
		defer en.h.SetPublicDomains(nil)
		if a := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", nil); a.status != 503 || a.header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("public domain: %d %v", a.status, a.header)
		}
		if a := en.do(t, "GET", flat+"nothing.duckdb_extension.gz", nil); a.status != 401 {
			t.Fatalf("a missing path under a public domain: %d", a.status)
		}
	})

	t.Run("well-known and health", func(t *testing.T) {
		a := en.do(t, "GET", "/acme/prod/.well-known/duckdb-extension-repo.json", nil)
		if a.status != 200 || !strings.Contains(string(a.body), "signature_keys") || a.header.Get("Cache-Control") != "public, no-cache" {
			t.Fatalf("well-known: %d %v", a.status, a.header)
		}
		if a := en.do(t, "GET", "/healthz", nil); a.status != 200 {
			t.Fatalf("healthz: %d", a.status)
		}
		en.h.SetReady(false)
		if a := en.do(t, "GET", "/readyz", nil); a.status != 503 {
			t.Fatalf("readyz not ready: %d", a.status)
		}
		en.h.SetReady(true)
		if a := en.do(t, "GET", "/readyz", nil); a.status != 200 {
			t.Fatalf("readyz: %d", a.status)
		}
	})

	t.Run("changes apply at the next request", func(t *testing.T) {
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", r11.ID, release.SetPublic); err != nil {
			t.Fatal(err)
		}
		a := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", nil)
		en.checkFile(t, a.body, true, r11)
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", r11.ID, release.Yank); err != nil {
			t.Fatal(err)
		}
		a = en.do(t, "GET", flat+"tresor.duckdb_extension.gz", nil)
		en.checkFile(t, a.body, true, r10)
		if a := en.do(t, "GET", "/acme/prod/tresor/1.1/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", nil); a.status != 401 {
			t.Fatalf("a yanked release: %d", a.status)
		}
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", r10.ID, release.Deprecate); err != nil {
			t.Fatal(err)
		}
		if a := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", nil); a.status != 401 {
			t.Fatalf("a deprecated current release on the flat path: %d", a.status)
		}
		if a := en.do(t, "GET", "/acme/prod/tresor/1.0/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", nil); a.status != 200 {
			t.Fatalf("a deprecated release on the versioned path: %d", a.status)
		}
	})

	t.Run("a suspended tenant is not served", func(t *testing.T) {
		ten := &tenants.Service{Store: en.st, Authz: authz.ServerAdmin{}}
		if _, err := ten.SetTenantState(ctx, admin, "acme", store.TenantSuspended); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = ten.SetTenantState(ctx, admin, "acme", store.TenantActive) }()
		for _, p := range []string{"/acme/prod/tresor/1.0/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", "/acme/prod/.well-known/duckdb-extension-repo.json"} {
			if a := en.do(t, "GET", p, nil); a.status != 404 {
				t.Fatalf("%s: %d", p, a.status)
			}
		}
	})

	t.Run("a stream corrupted mid-response aborts the connection", func(t *testing.T) {
		recs, err := en.st.ListReleases(ctx, mustChannel(t, en).ID, "tresor")
		if err != nil {
			t.Fatal(err)
		}
		var hash string
		for _, r := range recs {
			if r.ID == r10.ID {
				hash = r.BodyHash
			}
		}
		b, err := en.st.GetBlob(ctx, "default", hash)
		if err != nil {
			t.Fatal(err)
		}
		key := blob.StreamKey(b.StreamHash)
		rd, err := en.fsStore.Get(ctx, key, 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		obj, _ := io.ReadAll(rd)
		rd.Close()
		obj[len(obj)-10] ^= 1
		if err := en.fsStore.Put(ctx, key, bytes.NewReader(obj), int64(len(obj))); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"tresor.duckdb_extension", "tresor.duckdb_extension.gz"} {
			resp, err := http.Get(en.srv.URL + "/acme/prod/tresor/1.0/v2.0.0/linux_amd64/" + name)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			// the first request aborts mid-body; once corrupt_at is recorded, the next one is a 503
			if err == nil && resp.StatusCode != 503 {
				t.Fatalf("%s: a corrupted stream was sent as a clean response (%d)", name, resp.StatusCode)
			}
		}
		if a := en.do(t, "GET", "/acme/prod/tresor/1.0/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", nil); a.status != 503 {
			t.Fatalf("after the corruption was recorded: %d", a.status)
		}
	})
}

func mustChannel(t *testing.T, en *env) store.Channel {
	t.Helper()
	c, err := en.st.GetChannel(ctx, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sameHeaders(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if strings.Join(a.Values(k), "\n") != strings.Join(b.Values(k), "\n") {
			return false
		}
	}
	return true
}

func itoa(n int) string { return strconv.Itoa(n) }

func bufioReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }

func pemEncode(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// The http scheme: no Vary on public answers, and the limits.
func TestLimitsAndScheme(t *testing.T) {
	en := newEnv(t, Options{MaxDownloads: 1, MaxDownloadsPerClient: 1, MinRate: 1024, WriteIdleTimeout: 10 * time.Second}, "http")
	en.add(t, ext(t, 1000, 1, cpp("1.0")), release.AddOptions{Name: "tresor"})
	a := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", nil)
	if a.status != 200 || a.header.Get("Vary") != "" {
		t.Fatalf("http: %d %v", a.status, a.header)
	}
	rel, ok := en.h.acquire("1.2.3.4")
	if !ok {
		t.Fatal("first slot")
	}
	if _, ok := en.h.acquire("1.2.3.4"); ok {
		t.Fatal("a second slot for one client")
	}
	if _, ok := en.h.acquire("5.6.7.8"); ok {
		t.Fatal("a slot beyond the replica's limit")
	}
	b := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", nil)
	if b.status != 503 || b.header.Get("Retry-After") == "" || b.header.Get("ETag") != "" {
		t.Fatalf("at the limit: %d %v", b.status, b.header)
	}
	if h := en.do(t, "HEAD", flat+"tresor.duckdb_extension.gz", nil); h.status != 200 {
		t.Fatalf("a HEAD takes no slot: %d", h.status)
	}
	rel()
	if c := en.do(t, "GET", flat+"tresor.duckdb_extension.gz", nil); c.status != 200 {
		t.Fatalf("after release: %d", c.status)
	}
}

func TestClientAddr(t *testing.T) {
	h := &Handler{o: Options{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}
	for _, c := range []struct{ remote, xff, want string }{
		{"1.2.3.4:5", "9.9.9.9", "1.2.3.4"},
		{"10.0.0.1:5", "9.9.9.9", "9.9.9.9"},
		{"10.0.0.1:5", "6.6.6.6, 9.9.9.9, 10.0.0.2", "9.9.9.9"},
		{"10.0.0.1:5", "", "10.0.0.1"},
		{"10.0.0.1:5", "garbage", "10.0.0.1"},
		{"10.0.0.1:5", "9.9.9.9:443", "9.9.9.9"},
		{"10.0.0.1:5", "[2001:db8::1]:443", "2001:db8::1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := h.clientAddr(r); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
}

// A dual listener serves TLS and plain http on one port; a behind_proxy listener downgrades on
// X-Forwarded-Proto.
func TestListeners(t *testing.T) {
	en := newEnv(t, Options{}, "https")
	en.add(t, ext(t, 1000, 1, cpp("1.0")), release.AddOptions{Name: "tresor"})
	certFile, keyFile := selfSigned(t)
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &Server{Handler: en.h, Log: slog.New(slog.DiscardHandler), DrainTimeout: time.Millisecond, ShutdownTimeout: time.Second,
		Listeners: []Listener{
			{Listener: config.Listener{Addr: ln1.Addr().String(), Scheme: "dual", TLS: &config.TLSConfig{CertFile: certFile, KeyFile: keyFile}}, Net: ln1},
			{Listener: config.Listener{Addr: ln2.Addr().String(), Scheme: "https", BehindProxy: true}, Net: ln2},
		}}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Run(rctx) }()
	defer func() { cancel(); <-done }()
	time.Sleep(50 * time.Millisecond)

	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	vary := func(c *http.Client, url string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("GET", url+flat+"tresor.duckdb_extension.gz", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Vary")
	}
	if s, v := vary(tlsClient, "https://"+ln1.Addr().String(), nil); s != 200 || v != "Authorization" {
		t.Fatalf("dual over TLS: %d %q", s, v)
	}
	if s, v := vary(http.DefaultClient, "http://"+ln1.Addr().String(), nil); s != 200 || v != "" {
		t.Fatalf("dual over http: %d %q", s, v)
	}
	if s, v := vary(http.DefaultClient, "http://"+ln2.Addr().String(), map[string]string{"X-Forwarded-Proto": "https"}); s != 200 || v != "Authorization" {
		t.Fatalf("behind proxy, https: %d %q", s, v)
	}
	if s, v := vary(http.DefaultClient, "http://"+ln2.Addr().String(), map[string]string{"X-Forwarded-Proto": "http"}); s != 200 || v != "" {
		t.Fatalf("behind proxy, downgraded: %d %q", s, v)
	}
	if s, v := vary(http.DefaultClient, "http://"+ln2.Addr().String(), nil); s != 200 || v != "" {
		t.Fatalf("behind proxy, no header: %d %q", s, v)
	}
	for _, xfp := range []string{"https, https", "HTTPS", "https,http"} {
		if s, v := vary(http.DefaultClient, "http://"+ln2.Addr().String(), map[string]string{"X-Forwarded-Proto": xfp}); s != 200 || v != "" {
			t.Fatalf("behind proxy, X-Forwarded-Proto %q is a downgrade: %d %q", xfp, s, v)
		}
	}
	// a client that connects to the dual port and sends nothing does not hold up others
	idle, err := net.Dial("tcp", ln1.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	quick := &http.Client{Timeout: 3 * time.Second}
	resp, err := quick.Get("http://" + ln1.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("the dual port is stalled by an idle connection: %v", err)
	}
	resp.Body.Close()
	// draining: /readyz fails for good, whatever readiness says
	en.h.SetStopping()
	en.h.SetReady(true)
	if a := en.do(t, "GET", "/readyz", nil); a.status != 503 {
		t.Fatalf("readyz while stopping: %d", a.status)
	}
}

func selfSigned(t *testing.T) (string, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	cert := srv.TLS.Certificates[0]
	dir := t.TempDir()
	certPEM := pemEncode("CERTIFICATE", cert.Certificate[0])
	der, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pemEncode("PRIVATE KEY", der)
	cf, kf := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(cf, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kf, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return cf, kf
}

// A behind_proxy listener with trusted proxies refuses requests from other peers.
func TestBehindProxyRefusesPeers(t *testing.T) {
	en := newEnv(t, Options{}, "https")
	en.h.o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &Server{Handler: en.h, Log: slog.New(slog.DiscardHandler), DrainTimeout: time.Millisecond, ShutdownTimeout: time.Second,
		Listeners: []Listener{{Listener: config.Listener{Addr: ln.Addr().String(), Scheme: "https", BehindProxy: true}, Net: ln}}}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Run(rctx) }()
	defer func() { cancel(); <-done }()
	time.Sleep(50 * time.Millisecond)
	if _, err := http.Get("http://" + ln.Addr().String() + "/healthz"); err == nil {
		t.Fatal("a peer outside trusted_proxies was served")
	}
}
