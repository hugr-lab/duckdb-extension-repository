package gcs_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/blobtest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/gcs"
)

const bucket = "kista-test"

func tokens() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"})
}

// endpoint is KISTA_TEST_GCS: fake-gcs-server's http://127.0.0.1:<port>, its bucket created here.
func endpoint(t *testing.T) string {
	t.Helper()
	ep := os.Getenv("KISTA_TEST_GCS")
	if ep == "" {
		if os.Getenv("KISTA_TEST_REQUIRE_BLOB") == "1" {
			t.Fatal("KISTA_TEST_GCS is required")
		}
		t.Skip("KISTA_TEST_GCS not set")
	}
	body := strings.NewReader(`{"name":"` + bucket + `"}`)
	resp, err := http.Post(ep+"/storage/v1/b?project=test", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		t.Fatalf("creating the bucket: %d", resp.StatusCode)
	}
	// fake-gcs-server takes any caller: in front of it, a proxy refuses one without a token, as GCS
	// does for a private bucket, and keeps the Host so resumable sessions point back at the proxy
	target, _ := url.Parse(ep)
	rp := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	rp.ModifyResponse = func(resp *http.Response) error { // the session URI names fake-gcs-server's address
		if loc := resp.Header.Get("Location"); loc != "" {
			resp.Header.Set("Location", strings.Replace(loc, ep, front.URL, 1))
		}
		return nil
	}
	return front.URL
}

func config(ep string) gcs.Config {
	return gcs.Config{Bucket: bucket, Prefix: "t-" + uuid.NewString() + "/", Endpoint: ep, Tokens: tokens(), ChunkSize: 256 << 10}
}

func newStore(t *testing.T, cfg gcs.Config) blob.Store {
	t.Helper()
	s, err := gcs.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContract(t *testing.T) {
	ep := endpoint(t)
	blobtest.Run(t, blobtest.Harness{
		New: func(t *testing.T) blob.Store { return newStore(t, config(ep)) },
		Steered: func(t *testing.T) blob.Store {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent.json")
			t.Setenv("STORAGE_EMULATOR_HOST", "127.0.0.1:1")
			t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
			t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
			t.Setenv("NO_PROXY", "")
			return newStore(t, config(ep))
		},
		Failing: func(t *testing.T) (blob.Store, []string) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprintf(w, `{"error":{"code":403,"message":"%s %s","errors":[{"reason":"forbidden%s","message":"%s"}]}}`,
					blobtest.Secret, r.URL, blobtest.Secret, blobtest.Secret)
			}))
			t.Cleanup(srv.Close)
			cfg := config(srv.URL)
			cfg.Bucket, cfg.Prefix = "kista-failing", "secret-prefix/"
			return newStore(t, cfg), []string{strings.TrimPrefix(srv.URL, "http://"), "kista-failing", "secret-prefix"}
		},
		WithTimeout: func(t *testing.T, timeout time.Duration) blob.Store {
			cfg := config(ep)
			cfg.Timeout = timeout
			return newStore(t, cfg)
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
					t.Cleanup(func() { c.Close() })
				}
			}()
			cfg := config("http://" + ln.Addr().String())
			cfg.Bucket, cfg.Prefix, cfg.Timeout = "kista-stalled", "secret-prefix/", time.Second
			return newStore(t, cfg), []string{ln.Addr().String(), "kista-stalled", "secret-prefix"}
		},
	})
}

// A resumable upload against fake-gcs-server: several chunks, then the whole object reads back.
func TestResumableLive(t *testing.T) {
	ep := endpoint(t)
	s := newStore(t, config(ep))
	data := blobtest.Data(3<<20+12345, 7) // 13 chunks of 256 KiB, the last partial
	key := blobtest.Key(data)
	if err := s.Put(context.Background(), key, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes", len(got))
	}
}

// fakeGCS speaks the parts of GCS's protocol fake-gcs-server does not emulate: names escaped with
// %2F, a resumable session that keeps fewer bytes than sent, a chunk that fails and the status query
// after it, cancellation, and the answer's CRC32C.
type fakeGCS struct {
	mu         sync.Mutex
	data       []byte
	keep       int // bytes of the next chunk to drop (acknowledging fewer)
	failNext   bool
	badCRC     bool
	otherHost  string
	paths      []string
	ranges     []string // the Content-Range of every session PUT
	deleted    int
	srv        *httptest.Server
	session    string
	retry429   bool // the next chunk answers 429 with Retry-After: 1
	alwaysFail bool // every session PUT answers 503
	forbidden  bool // every chunk answers 403
	pages      int  // List answers this many pages
}

func (f *fakeGCS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.EscapedPath())
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	crc := func() string {
		if f.badCRC {
			return "AAAAAA=="
		}
		var m struct{ CRC32C string }
		_ = json.Unmarshal([]byte(f.session), &m)
		return m.CRC32C
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Query().Get("uploadType") == "resumable":
		b, _ := io.ReadAll(r.Body)
		f.session = string(b)
		host := "http://" + r.Host
		if f.otherHost != "" {
			host = f.otherHost
		}
		w.Header().Set("Location", host+"/upload/storage/v1/b/"+bucket+"/o?uploadType=resumable&upload_id=u1")
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut && r.URL.Query().Get("upload_id") == "u1":
		rng := r.Header.Get("Content-Range")
		f.ranges = append(f.ranges, rng)
		b, _ := io.ReadAll(r.Body)
		total, _ := strconv.Atoi(rng[strings.LastIndex(rng, "/")+1:])
		if f.failNext && len(b) > 0 || f.alwaysFail {
			f.failNext = false
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if f.forbidden && len(b) > 0 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if f.retry429 && len(b) > 0 {
			f.retry429 = false
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if len(b) > 0 {
			var from int
			_, _ = fmt.Sscanf(rng, "bytes %d-", &from)
			if from != len(f.data) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n := len(b) - f.keep
			f.keep = 0
			f.data = append(f.data, b[:n]...)
		}
		if len(f.data) == total {
			_ = json.NewEncoder(w).Encode(map[string]string{"size": strconv.Itoa(total), "crc32c": crc(), "generation": "17"})
			return
		}
		if len(f.data) > 0 {
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(f.data)-1))
		}
		w.WriteHeader(308)
	case r.Method == http.MethodDelete && r.URL.Query().Get("upload_id") == "u1":
		f.deleted++
		w.WriteHeader(499)
	case r.Method == http.MethodDelete:
		f.deleted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o"):
		page, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Query().Get("pageToken"), "page-"))
		out := map[string]any{"items": []map[string]string{{"name": "p/streams/" + strings.Repeat(strconv.Itoa(page), 64), "size": "1",
			"updated": "2026-10-14T10:00:00.123Z"}}}
		if page+1 < f.pages {
			out["nextPageToken"] = "page-" + strconv.Itoa(page+1)
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFake(t *testing.T) (*fakeGCS, *gcs.Store) {
	t.Helper()
	f := &fakeGCS{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	s, err := gcs.New(gcs.Config{Bucket: bucket, Prefix: "p/", Endpoint: f.srv.URL, Tokens: tokens(), ChunkSize: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	return f, s
}

func TestResumableProtocol(t *testing.T) {
	data := blobtest.Data(600<<10, 3) // 256 KiB, 256 KiB, 88 KiB
	key := blobtest.Key(data)
	const k = 256 << 10
	n := len(data)
	put := func(f *fakeGCS, s *gcs.Store) error {
		return s.Put(context.Background(), key, bytes.NewReader(data), int64(n))
	}
	// a short acknowledge: the next chunk starts right after what GCS kept, never anywhere else
	f, s := newFake(t)
	f.keep = 1000
	if err := put(f, s); err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf("bytes 0-%d/%d", k-1, n), fmt.Sprintf("bytes %d-%d/%d", k-1000, 2*k-1001, n),
		fmt.Sprintf("bytes %d-%d/%d", 2*k-1000, n-1, n)}
	if !bytes.Equal(f.data, data) || strings.Join(f.ranges, ";") != strings.Join(want, ";") {
		t.Fatalf("ranges %v, want %v", f.ranges, want)
	}
	// a failed chunk: a status query, then the chunk again
	f, s = newFake(t)
	f.failNext = true
	if err := put(f, s); err != nil {
		t.Fatal(err)
	}
	want = []string{fmt.Sprintf("bytes 0-%d/%d", k-1, n), fmt.Sprintf("bytes */%d", n), fmt.Sprintf("bytes 0-%d/%d", k-1, n),
		fmt.Sprintf("bytes %d-%d/%d", k, 2*k-1, n), fmt.Sprintf("bytes %d-%d/%d", 2*k, n-1, n)}
	if !bytes.Equal(f.data, data) || strings.Join(f.ranges, ";") != strings.Join(want, ";") {
		t.Fatalf("after a failed chunk: ranges %v, want %v", f.ranges, want)
	}
	// a 429 with Retry-After is waited out
	f, s = newFake(t)
	f.retry429 = true
	start := time.Now()
	if err := put(f, s); err != nil || time.Since(start) < time.Second {
		t.Fatalf("429: %v after %s", err, time.Since(start))
	}
	// an answer with another CRC32C: that generation is deleted, the Put fails
	f, s = newFake(t)
	f.badCRC = true
	if err := put(f, s); err == nil || f.deleted != 1 || !strings.Contains(strings.Join(f.paths, ";"), "DELETE") {
		t.Fatalf("a wrong CRC32C: %v (deleted %d)", err, f.deleted)
	}
	// a session on another host is refused before any byte goes to it
	f, s = newFake(t)
	f.otherHost = "http://127.0.0.1:1"
	if err := put(f, s); err == nil || len(f.ranges) != 0 {
		t.Fatalf("followed a session on another host: %v %v", err, f.ranges)
	}
	// a chunk refused for good (403): no status query, no retry; the session is cancelled
	f, s = newFake(t)
	f.forbidden = true
	if err := put(f, s); err == nil || len(f.ranges) != 1 || f.deleted != 1 {
		t.Fatalf("a refused chunk: %v, ranges %v, cancelled %d", err, f.ranges, f.deleted)
	}
	// every chunk failing: the upload stops and the session is cancelled (499)
	f, s = newFake(t)
	f.alwaysFail = true
	if err := put(f, s); err == nil || f.deleted != 1 {
		t.Fatalf("a failing session: %v, cancelled %d", err, f.deleted)
	}
}

// A small object is one multipart upload whose metadata carries the CRC32C, which GCS checks.
func TestMultipartUpload(t *testing.T) {
	data := blobtest.Data(100<<10, 5)
	var meta map[string]string
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if r.URL.Query().Get("uploadType") != "multipart" || err != nil || mt != "multipart/related" ||
			r.ContentLength <= int64(len(data)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		p, err := mr.NextPart()
		if err != nil || p.Header.Get("Content-Type") != "application/json; charset=UTF-8" || json.NewDecoder(p).Decode(&meta) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if p, err = mr.NextPart(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		got, _ = io.ReadAll(p)
		if _, err := mr.NextPart(); err != io.EOF {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"size": strconv.Itoa(len(got)), "crc32c": meta["crc32c"], "generation": "1"})
	}))
	defer srv.Close()
	s := newStore(t, gcs.Config{Bucket: bucket, Prefix: "p/", Endpoint: srv.URL, Tokens: tokens()})
	if err := s.Put(context.Background(), blobtest.Key(data), bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	sum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	want := base64.StdEncoding.EncodeToString([]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)})
	if !bytes.Equal(got, data) || meta["crc32c"] != want {
		t.Fatalf("body %d bytes, metadata %v, want crc32c %s", len(got), meta, want)
	}
}

// List follows every page.
func TestListPages(t *testing.T) {
	f, s := newFake(t)
	f.pages = 3
	var keys []string
	if err := s.List(context.Background(), "streams/", func(k string, _ int64, _ time.Time) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("listed %v", keys)
	}
}

// Object names are one path segment: "/" travels as %2F.
func TestEscapedNames(t *testing.T) {
	f, s := newFake(t)
	key := blobtest.Key([]byte("x"))
	_ = s.Delete(context.Background(), key)
	want := "DELETE /storage/v1/b/" + bucket + "/o/p%2F" + strings.ReplaceAll(key, "/", "%2F")
	if len(f.paths) != 1 || f.paths[0] != want {
		t.Fatalf("paths %v, want %s", f.paths, want)
	}
}

// The anonymous check: an unauthenticated read; 200 and 404 public, 401 and 403 private.
func TestAnonymous(t *testing.T) {
	for status, want := range map[int][2]bool{200: {true, true}, 404: {true, true}, 401: {false, true}, 403: {false, true}, 500: {false, false}, 400: {false, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(status)
		}))
		s, _ := gcs.New(gcs.Config{Bucket: bucket, Endpoint: srv.URL, Tokens: tokens()})
		readable, ok := s.Anonymous(context.Background(), blob.MarkerKey)
		srv.Close()
		if readable != want[0] || ok != want[1] {
			t.Errorf("%d: readable %v ok %v", status, readable, ok)
		}
	}
}

func TestConfigRefused(t *testing.T) {
	for name, c := range map[string]gcs.Config{
		"no bucket":  {Tokens: tokens()},
		"no tokens":  {Bucket: bucket},
		"prefix":     {Bucket: bucket, Tokens: tokens(), Prefix: "../"},
		"query":      {Bucket: bucket, Tokens: tokens(), Endpoint: "https://storage.googleapis.com/?x=1"},
		"path":       {Bucket: bucket, Tokens: tokens(), Endpoint: "https://storage.googleapis.com/b"},
		"chunk size": {Bucket: bucket, Tokens: tokens(), ChunkSize: 1000},
	} {
		if _, err := gcs.New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s, err := gcs.New(gcs.Config{Bucket: bucket, Tokens: tokens(), Prefix: "a/"})
	if err != nil || s.ID() != "gcs:storage.googleapis.com/"+bucket+"/a/" {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := s.Stat(context.Background(), "bad key"); !errors.Is(err, blob.ErrKey) {
		t.Fatal(err)
	}
}
