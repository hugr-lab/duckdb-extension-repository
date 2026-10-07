package e2e

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	blobfs "github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/s3"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0005: an extension is spooled, verified and committed into a storage domain, then served
// from the stored stream with a channel signature, every byte verified against the record.

// blobDomains returns the domains to run against: fs always, and S3 when KISTA_TEST_S3 is set
// (http://<access>:<secret>@<host:port>/<bucket>, a MinIO or SeaweedFS).
func blobDomains(t *testing.T) map[string]func(t *testing.T) blob.Store {
	out := map[string]func(t *testing.T) blob.Store{
		"fs": func(t *testing.T) blob.Store {
			s, err := blobfs.Open(filepath.Join(t.TempDir(), "blobs"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			return s
		},
	}
	v := os.Getenv("KISTA_TEST_S3")
	if v == "" {
		if os.Getenv("KISTA_TEST_REQUIRE_BLOB") == "1" {
			t.Fatal("KISTA_TEST_S3 is required")
		}
		return out
	}
	u, err := url.Parse(v)
	if err != nil || u.User == nil {
		t.Fatal("KISTA_TEST_S3 is http://<access>:<secret>@<host:port>/<bucket>")
	}
	out["s3"] = func(t *testing.T) blob.Store {
		pw, _ := u.User.Password()
		bucket := strings.Trim(u.Path, "/")
		c, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(u.User.Username(), pw, ""), Region: "us-east-1"})
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := c.BucketExists(context.Background(), bucket); err != nil {
			t.Fatal(err)
		} else if !ok {
			if err := c.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
				t.Fatal(err)
			}
		}
		dir := t.TempDir()
		write := func(name, v string) string {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}
		s, err := s3.New(s3.Config{Endpoint: u.Host, Bucket: bucket, Prefix: "e2e-" + uuid.NewString() + "/",
			Region: "us-east-1", PathStyle: true, SSE: "none",
			AccessKeyFile: write("a", u.User.Username()), SecretKeyFile: write("s", pw)})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	return out
}

// blobRepo is a channel served from the blob service.
type blobRepo struct {
	t     *testing.T
	b     *build
	svc   *blob.Service
	st    *store.Store
	store blob.Store

	mu    sync.Mutex
	files map[string]blobEntry // extension name → record and signature
	log   []request
	plain bool // serve the plain name; otherwise 404 for it
	noGz  bool // 404 for the .gz
	srv   *httptest.Server
}

type blobEntry struct {
	rec store.Blob
	sig []byte
}

func newBlobRepo(t *testing.T, b *build, s blob.Store, tlsOn bool) *blobRepo {
	t.Helper()
	st, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc, err := blob.NewService(context.Background(), st, []blob.Domain{{Name: "default", Kind: "test", Store: s}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 256 << 20, MaxIngests: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	r := &blobRepo{t: t, b: b, svc: svc, st: st, store: s, files: map[string]blobEntry{}}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	if tlsOn {
		r.srv.TLS = &tls.Config{Certificates: []tls.Certificate{testCA(t).leaf}}
		r.srv.StartTLS()
	} else {
		r.srv.Start()
	}
	t.Cleanup(r.srv.Close)
	return r
}

// add spools the build's extension, verifies its footer and its build signature (a fresh key
// signs the body hash; the upstream signature check of spec 0009 is modelled by verifying that),
// commits it, and keeps the channel signature for serving.
func (r *blobRepo) add(name string, k *key) {
	r.t.Helper()
	ctx := context.Background()
	f, err := os.Open(r.b.extensions[name])
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	sp, err := r.svc.Spool(ctx, "default", f)
	if err != nil {
		r.t.Fatal(err)
	}
	defer sp.Close()
	file := sp.File()
	if file.Metadata.Platform != r.b.platform {
		r.t.Fatalf("footer platform %q, want %q", file.Metadata.Platform, r.b.platform)
	}
	sig, err := k.signer.Sign(ctx, file.Hash)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, ok := extfile.Verify(file.Hash, sig, []*rsa.PublicKey{k.signer.Public()}); !ok {
		r.t.Fatal("the channel signature does not verify")
	}
	rec, err := sp.Commit(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	r.mu.Lock()
	r.files[name] = blobEntry{rec, sig}
	r.mu.Unlock()
}

func (r *blobRepo) URL() string { return r.srv.URL }

func (r *blobRepo) requests() []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]request(nil), r.log...)
}

func (r *blobRepo) serve(w http.ResponseWriter, req *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	defer func() {
		r.mu.Lock()
		r.log = append(r.log, request{Method: req.Method, Path: req.URL.Path, Range: req.Header.Get("Range"), Status: rec.status})
		r.mu.Unlock()
	}()
	prefix := "/" + r.b.versionDir + "/" + r.b.platform + "/"
	name, ok := strings.CutPrefix(req.URL.Path, prefix)
	if !ok {
		rec.WriteHeader(http.StatusNotFound)
		return
	}
	gz := strings.HasSuffix(name, ".duckdb_extension.gz")
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), ".duckdb_extension")
	r.mu.Lock()
	e, found := r.files[name]
	plainOK, gzOK := r.plain, !r.noGz
	r.mu.Unlock()
	if !found || gz && !gzOK || !gz && !plainOK {
		rec.WriteHeader(http.StatusNotFound)
		return
	}
	ctx := req.Context()
	if gz {
		f, err := r.svc.OpenGzip(ctx, e.rec, e.sig)
		if err != nil {
			rec.WriteHeader(http.StatusInternalServerError)
			return
		}
		rd := f.NewReader(ctx)
		defer rd.Close()
		rec.Header().Set("Content-Type", f.ContentType)
		rec.Header().Set("ETag", f.ETag)
		http.ServeContent(rec, req, "", time.Time{}, readerAbort{rd})
		return
	}
	// the plain name: whole, no Accept-Ranges
	f, err := r.svc.OpenPlain(ctx, e.rec, e.sig)
	if err != nil {
		rec.WriteHeader(http.StatusInternalServerError)
		return
	}
	rec.Header().Set("Content-Type", f.ContentType)
	rec.Header().Set("ETag", f.ETag)
	rec.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	rec.WriteHeader(http.StatusOK)
	if req.Method == http.MethodHead {
		return
	}
	rd := f.NewReader(ctx)
	defer rd.Close()
	if _, err := io.Copy(rec, rd); errors.Is(err, blob.ErrCorrupt) {
		panic(http.ErrAbortHandler)
	}
}

// readerAbort turns ErrCorrupt into an aborted connection (spec 0006 does the same).
type readerAbort struct{ blob.Reader }

func (r readerAbort) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if errors.Is(err, blob.ErrCorrupt) {
		panic(http.ErrAbortHandler)
	}
	return n, err
}

func TestBlobServing(t *testing.T) {
	b := needBuild(t)
	for kind, newStore := range blobDomains(t) {
		t.Run(kind, func(t *testing.T) {
			k := newKey(t)

			// over http:// with the built-in client: one GET of the .gz
			t.Run("http", func(t *testing.T) {
				r := newBlobRepo(t, b, newStore(t), false)
				r.add("loadable_extension_demo", k)
				res := newSession(t, b, nil).exec(
					createRepo("r", r.URL(), k.pem),
					"INSTALL loadable_extension_demo FROM r",
					"LOAD loadable_extension_demo FROM r",
					"SELECT test_alias_hello()",
				)
				mustOK(t, res)
				log := r.requests()
				if len(log) != 1 || log[0].Method != "GET" || log[0].Path != b.flatPath("loadable_extension_demo")+".gz" || log[0].Status != 200 {
					t.Fatalf("requests: %+v", log)
				}
			})

			// over https:// through httpfs: HEAD, then a ranged GET
			t.Run("https", func(t *testing.T) {
				r := newBlobRepo(t, b, newStore(t), true)
				r.add("loadable_extension_demo", k)
				stmts := append(bootstrapHTTPFS(t, b, k),
					createRepo("r", r.URL(), k.pem),
					"INSTALL loadable_extension_demo FROM r",
					"LOAD loadable_extension_demo FROM r",
					"SELECT test_alias_hello()",
				)
				mustOK(t, newSession(t, b, nil).exec(stmts...))
				gz := b.flatPath("loadable_extension_demo") + ".gz"
				log := r.requests()
				if len(log) != 2 || log[0].Method != "HEAD" || log[0].Path != gz ||
					log[1].Method != "GET" || log[1].Range == "" || log[1].Status != 206 {
					t.Fatalf("requests: %+v", log)
				}
			})

			// with the .gz missing, httpfs falls back to the plain name, served whole
			t.Run("plain", func(t *testing.T) {
				r := newBlobRepo(t, b, newStore(t), true)
				r.add("loadable_extension_demo", k)
				r.plain, r.noGz = true, true
				stmts := append(bootstrapHTTPFS(t, b, k),
					createRepo("r", r.URL(), k.pem),
					"INSTALL loadable_extension_demo FROM r",
					"LOAD loadable_extension_demo FROM r",
					"SELECT test_alias_hello()",
				)
				mustOK(t, newSession(t, b, nil).exec(stmts...))
				var plain bool
				for _, q := range r.requests() {
					t.Logf("%s %s range=%q status=%d", q.Method, q.Path, q.Range, q.Status)
					plain = plain || q.Path == b.flatPath("loadable_extension_demo") && q.Method == "GET" && q.Status == 200
				}
				if !plain {
					t.Fatal("the plain name was not fetched")
				}
			})

			// a tampered stored stream: the connection is aborted and nothing is installed
			t.Run("tampered", func(t *testing.T) {
				s := newStore(t)
				r := newBlobRepo(t, b, s, false)
				r.add("loadable_extension_demo", k)
				e := r.files["loadable_extension_demo"]
				key := blob.StreamKey(e.rec.StreamHash)
				rd, err := s.Get(context.Background(), key, 0, -1)
				if err != nil {
					t.Fatal(err)
				}
				obj, _ := io.ReadAll(rd)
				rd.Close()
				obj[len(obj)/2] ^= 1
				if err := s.Put(context.Background(), key, strings.NewReader(string(obj)), int64(len(obj))); err != nil {
					t.Fatal(err)
				}
				res := newSession(t, b, nil).exec(
					createRepo("r", r.URL(), k.pem),
					"INSTALL loadable_extension_demo FROM r",
				)
				mustOK(t, res[:1])
				if res[1].OK {
					t.Fatal("installed from a tampered stream")
				}
				t.Logf("tampered: %s", res[1].Error)
				got, err := r.st.GetBlob(context.Background(), "default", e.rec.BodyHash)
				if err != nil || got.CorruptAt.IsZero() {
					t.Fatalf("corrupt_at not set: %v", err)
				}
			})
		})
	}
}
