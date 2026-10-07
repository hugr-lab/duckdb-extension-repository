package s3_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/blobtest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/s3"
)

// server is KISTA_TEST_S3: http://<access>:<secret>@127.0.0.1:<port>/<bucket> (MinIO, SeaweedFS).
func server(t *testing.T) *url.URL {
	t.Helper()
	v := os.Getenv("KISTA_TEST_S3")
	if v == "" {
		if os.Getenv("KISTA_TEST_REQUIRE_BLOB") == "1" {
			t.Fatal("KISTA_TEST_S3 is required")
		}
		t.Skip("KISTA_TEST_S3 not set")
	}
	u, err := url.Parse(v)
	if err != nil || u.User == nil || len(u.Path) < 2 {
		t.Fatal("KISTA_TEST_S3 is http://<access>:<secret>@<host:port>/<bucket>")
	}
	return u
}

func secretFile(t *testing.T, v string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(v+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func config(t *testing.T, u *url.URL) s3.Config {
	t.Helper()
	pw, _ := u.User.Password()
	return s3.Config{
		Endpoint: u.Host, Bucket: strings.Trim(u.Path, "/"), Region: "us-east-1", PathStyle: true, SSE: "none",
		Prefix:        "t-" + uuid.NewString() + "/",
		AccessKeyFile: secretFile(t, u.User.Username(), 0o600),
		SecretKeyFile: secretFile(t, pw, 0o600),
	}
}

var bucketReady = map[string]bool{}

func ensureBucket(t *testing.T, u *url.URL) {
	t.Helper()
	if bucketReady[u.String()] {
		return
	}
	pw, _ := u.User.Password()
	c, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(u.User.Username(), pw, ""), Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	bucket := strings.Trim(u.Path, "/")
	ctx := context.Background()
	ok, err := c.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
			t.Fatal(err)
		}
	}
	bucketReady[u.String()] = true
}

func newStore(t *testing.T, cfg s3.Config) blob.Store {
	t.Helper()
	s, err := s3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContract(t *testing.T) {
	u := server(t)
	ensureBucket(t, u)
	blobtest.Run(t, blobtest.Harness{
		New: func(t *testing.T) blob.Store { return newStore(t, config(t, u)) },
		Steered: func(t *testing.T) blob.Store {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(home, ".aws", "credentials"),
				[]byte("[default]\naws_access_key_id = wrong\naws_secret_access_key = wrong\n"), 0o600)
			t.Setenv("HOME", home)
			for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
				"MINIO_ACCESS_KEY", "MINIO_SECRET_KEY", "MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD"} {
				t.Setenv(k, "wrong")
			}
			t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
			t.Setenv("AWS_REGION", "eu-north-1")
			t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
			t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
			t.Setenv("NO_PROXY", "")
			return newStore(t, config(t, u))
		},
		Stalling: func(t *testing.T) (blob.Store, []string) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					t.Cleanup(func() { c.Close() }) // accept, never read or answer
				}
			}()
			cfg := config(t, u)
			cfg.Endpoint, cfg.Bucket, cfg.Prefix = ln.Addr().String(), "kista-stalled", "secret-prefix/"
			cfg.Timeout = time.Second
			return newStore(t, cfg), []string{ln.Addr().String(), "kista-stalled", "secret-prefix"}
		},
		Failing: func(t *testing.T) (blob.Store, []string) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				w.Header().Set("x-amz-request-id", blobtest.Secret)
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>AccessDenied ` + blobtest.Secret +
					`</Code><Message>` + blobtest.Secret + `</Message><Resource>` + r.URL.Path +
					`</Resource><Key>` + r.URL.Path + `</Key><RequestId>` + blobtest.Secret + `</RequestId></Error>`))
			}))
			t.Cleanup(srv.Close)
			cfg := config(t, u)
			cfg.Endpoint = strings.TrimPrefix(srv.URL, "http://")
			cfg.Bucket, cfg.Prefix = "kista-failing", "secret-prefix/"
			return newStore(t, cfg), []string{cfg.Endpoint, "kista-failing", "secret-prefix"}
		},
	})
}

func TestWrongCredentials(t *testing.T) {
	u := server(t)
	ensureBucket(t, u)
	cfg := config(t, u)
	cfg.SecretKeyFile = secretFile(t, "not-the-secret", 0o600)
	s := newStore(t, cfg)
	_, err := s.Stat(context.Background(), blobtest.Key([]byte("x")))
	if err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("wrong credentials: %v", err)
	}
}

func TestCredentialFileMode(t *testing.T) {
	cfg := s3.Config{Endpoint: "127.0.0.1:1", Bucket: "kista", Region: "us-east-1", PathStyle: true,
		AccessKeyFile: secretFile(t, "a", 0o644), SecretKeyFile: secretFile(t, "s", 0o600)}
	s := newStore(t, cfg)
	_, err := s.Stat(context.Background(), blobtest.Key([]byte("x")))
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("a world-readable credential file was used: %v", err)
	}
}

func TestConfigRefused(t *testing.T) {
	good := s3.Config{Endpoint: "e:1", Bucket: "kista", Region: "r", AccessKeyFile: "/a", SecretKeyFile: "/s"}
	for name, mut := range map[string]func(*s3.Config){
		"no region":   func(c *s3.Config) { c.Region = "" },
		"no bucket":   func(c *s3.Config) { c.Bucket = "" },
		"no keys":     func(c *s3.Config) { c.AccessKeyFile = "" },
		"bad prefix":  func(c *s3.Config) { c.Prefix = "../" },
		"upper":       func(c *s3.Config) { c.Prefix = "A/" },
		"no slash":    func(c *s3.Config) { c.Prefix = "a" },
		"unknown sse": func(c *s3.Config) { c.SSE = "aes" },
	} {
		c := good
		mut(&c)
		if _, err := s3.New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := s3.New(good); err != nil {
		t.Fatal(err)
	}
}

// The anonymous check reports a public bucket.
func TestAnonymousPublic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	s := newStore(t, s3.Config{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Bucket: "kista", Region: "r", PathStyle: true,
		AccessKeyFile: "/a", SecretKeyFile: "/s"})
	if readable, ok := s.Anonymous(context.Background(), blob.MarkerKey); !ok || !readable {
		t.Fatalf("public bucket: readable %v ok %v", readable, ok)
	}
}

// A cancelled multipart upload is aborted on the server, not left pending.
func TestCancelledMultipartAborted(t *testing.T) {
	u := server(t)
	ensureBucket(t, u)
	cfg := config(t, u)
	s := newStore(t, cfg)
	data := blobtest.Data(40<<20, 7)
	ctx, cancel := context.WithCancel(context.Background())
	// cancel while the second part is being read: the first part is already uploaded
	r := readerAtFunc(func(p []byte, off int64) (int, error) {
		if off >= 20<<20 {
			cancel()
		}
		return strings.NewReader(string(data)).ReadAt(p, off)
	})
	if err := s.Put(ctx, blobtest.Key(data), r, int64(len(data))); err == nil {
		t.Fatal("a cancelled upload succeeded")
	}
	pw, _ := u.User.Password()
	c, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(u.User.Username(), pw, ""), Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	for up := range c.ListIncompleteUploads(context.Background(), strings.Trim(u.Path, "/"), cfg.Prefix, true) {
		if up.Err != nil {
			t.Fatal(up.Err)
		}
		t.Fatalf("a pending upload was left: %s", up.Key)
	}
}

type readerAtFunc func([]byte, int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }
