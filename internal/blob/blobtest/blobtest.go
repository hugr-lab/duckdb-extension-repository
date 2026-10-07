// Package blobtest is the contract suite every object-store backend runs (spec 0005, "Testing").
package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
)

// Secret must never appear in an error.
const Secret = "hunter2-planted-secret"

// Harness provides a backend.
type Harness struct {
	// New returns a fresh, empty store (its own prefix or directory).
	New func(t *testing.T) blob.Store
	// Steered, if set, returns a store built while steering variables (AWS_*, MINIO_*, HOME with a
	// planted ~/.aws) point elsewhere; it must behave exactly like New.
	Steered func(t *testing.T) blob.Store
	// Failing, if set, returns a store whose server answers every call with an error body
	// containing Secret, and strings no error may contain (host, bucket, prefix).
	Failing func(t *testing.T) (blob.Store, []string)
	// Stalling, if set, returns a store whose server accepts connections and then never answers or
	// stops mid-body, and strings no error may contain. Every call must fail within a few seconds.
	Stalling func(t *testing.T) (blob.Store, []string)
}

var ctx = context.Background()

// Key returns the stream key of data.
func Key(data []byte) string {
	h := sha256.Sum256(data)
	return blob.StreamKey(hex.EncodeToString(h[:]))
}

// Data returns n deterministic bytes.
func Data(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed) + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

func get(t *testing.T, s blob.Store, key string, off, n int64) []byte {
	t.Helper()
	r, err := s.Get(ctx, key, off, n)
	if err != nil {
		t.Fatalf("get %s [%d,%d): %v", key, off, n, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return b
}

// failingReader fails after n bytes.
type failingReader struct {
	data []byte
	n    int64
}

func (f failingReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.n {
		return 0, errors.New("interrupted")
	}
	if int64(len(p)) > f.n-off {
		k := copy(p, f.data[off:f.n])
		return k, errors.New("interrupted")
	}
	return copy(p, f.data[off:]), nil
}

// Run runs the contract suite.
func Run(t *testing.T, h Harness) {
	t.Run("put get stat list delete", func(t *testing.T) {
		s := h.New(t)
		data := Data(3<<20+17, 1)
		key := Key(data)
		before := time.Now().Add(-time.Hour)
		if err := s.Put(ctx, key, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
		if n, err := s.Stat(ctx, key); err != nil || n != int64(len(data)) {
			t.Fatalf("stat: %d %v", n, err)
		}
		if !bytes.Equal(get(t, s, key, 0, -1), data) {
			t.Fatal("whole read differs")
		}
		var seen []string
		err := s.List(ctx, "streams/", func(k string, size int64, mod time.Time) error {
			seen = append(seen, k)
			if k == key && (size != int64(len(data)) || mod.Before(before) || mod.After(time.Now().Add(time.Hour))) {
				t.Errorf("list: size %d, modified %s", size, mod)
			}
			return nil
		})
		if err != nil || len(seen) != 1 || seen[0] != key {
			t.Fatalf("list: %v %v", seen, err)
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stat(ctx, key); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("stat after delete: %v", err)
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Fatalf("deleting a missing object: %v", err)
		}
	})

	t.Run("ranges at chunk boundaries", func(t *testing.T) {
		s := h.New(t)
		const mib = 1 << 20
		data := Data(2*mib+5, 2)
		key := Key(data)
		if err := s.Put(ctx, key, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
		for _, r := range [][2]int64{{0, 1}, {mib - 1, 2}, {mib, mib}, {mib + 3, -1}, {2 * mib, 5}, {2*mib + 4, 1}, {10, 0}} {
			want := data[r[0]:]
			if r[1] >= 0 {
				want = data[r[0] : r[0]+r[1]]
			}
			if got := get(t, s, key, r[0], r[1]); !bytes.Equal(got, want) {
				t.Fatalf("range %v: %d bytes, want %d", r, len(got), len(want))
			}
		}
	})

	t.Run("put replaces", func(t *testing.T) {
		s := h.New(t)
		key := blob.MarkerKey
		for _, v := range []string{"first version, longer", "second"} {
			if err := s.Put(ctx, key, strings.NewReader(v), int64(len(v))); err != nil {
				t.Fatal(err)
			}
			if got := get(t, s, key, 0, -1); string(got) != v {
				t.Fatalf("got %q, want %q", got, v)
			}
		}
	})

	t.Run("refused keys and prefixes", func(t *testing.T) {
		s := h.New(t)
		bad := []string{"", "streams/", "streams/ABC", "streams/" + strings.Repeat("A", 64),
			"streams/../" + strings.Repeat("a", 64), "/streams/" + strings.Repeat("a", 64),
			"tmp/x", "other/" + strings.Repeat("a", 64), "streams/" + strings.Repeat("a", 64) + "/x",
			"Kista-domain.json", "kista-domain.json/"}
		for _, k := range bad {
			if err := s.Put(ctx, k, strings.NewReader("x"), 1); !errors.Is(err, blob.ErrKey) {
				t.Errorf("put %q: %v", k, err)
			}
			if _, err := s.Get(ctx, k, 0, -1); !errors.Is(err, blob.ErrKey) {
				t.Errorf("get %q: %v", k, err)
			}
			if _, err := s.Stat(ctx, k); !errors.Is(err, blob.ErrKey) {
				t.Errorf("stat %q: %v", k, err)
			}
			if err := s.Delete(ctx, k); !errors.Is(err, blob.ErrKey) {
				t.Errorf("delete %q: %v", k, err)
			}
		}
		for _, p := range []string{"", "streams", "/", "kista-domain.json", "../", "streams/x"} {
			if err := s.List(ctx, p, func(string, int64, time.Time) error { return nil }); !errors.Is(err, blob.ErrKey) {
				t.Errorf("list %q: %v", p, err)
			}
		}
	})

	t.Run("missing object", func(t *testing.T) {
		s := h.New(t)
		key := Key([]byte("never stored"))
		if _, err := s.Stat(ctx, key); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("stat: %v", err)
		}
		if _, err := s.Get(ctx, key, 0, -1); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("get: %v", err)
		}
	})

	t.Run("interrupted put leaves nothing visible", func(t *testing.T) {
		s := h.New(t)
		data := Data(1<<20, 3)
		key := Key(data)
		err := s.Put(ctx, key, failingReader{data, 1000}, int64(len(data)))
		if err == nil {
			t.Fatal("put of a failing reader succeeded")
		}
		if _, err := s.Stat(ctx, key); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("an interrupted put is visible: %v", err)
		}
		// a replace that fails keeps the old object
		if err := s.Put(ctx, key, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
		if err := s.Put(ctx, key, failingReader{data, 1000}, int64(len(data))); err == nil {
			t.Fatal("put of a failing reader succeeded")
		}
		if !bytes.Equal(get(t, s, key, 0, -1), data) {
			t.Fatal("an interrupted replace changed the object")
		}
	})

	t.Run("concurrent puts of one key", func(t *testing.T) {
		s := h.New(t)
		data := Data(2<<20+1, 4)
		key := Key(data)
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- s.Put(ctx, key, bytes.NewReader(data), int64(len(data)))
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(get(t, s, key, 0, -1), data) {
			t.Fatal("concurrent puts left other bytes")
		}
	})

	t.Run("list tmp", func(t *testing.T) {
		s := h.New(t)
		key := "tmp/" + uuid.NewString()
		if err := s.Put(ctx, key, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
		found := false
		if err := s.List(ctx, "tmp/", func(k string, _ int64, _ time.Time) error {
			found = found || k == key
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("tmp object not listed")
		}
		stop := errors.New("stop")
		if err := s.List(ctx, "tmp/", func(string, int64, time.Time) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("list did not return the callback's error: %v", err)
		}
	})

	t.Run("not anonymous", func(t *testing.T) {
		s := h.New(t)
		if err := s.Put(ctx, blob.MarkerKey, strings.NewReader("{}"), 2); err != nil {
			t.Fatal(err)
		}
		if readable, ok := s.Anonymous(ctx, blob.MarkerKey); !ok || readable {
			t.Fatalf("anonymous: readable %v, ok %v", readable, ok)
		}
	})

	t.Run("id", func(t *testing.T) {
		a, b := h.New(t), h.New(t)
		if a.ID() == "" || a.ID() == b.ID() {
			t.Fatalf("ids %q and %q", a.ID(), b.ID())
		}
	})

	t.Run("steering variables are ignored", func(t *testing.T) {
		if h.Steered == nil {
			t.Skip("not applicable")
		}
		s := h.Steered(t)
		data := Data(100, 5)
		key := Key(data)
		if err := s.Put(ctx, key, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(get(t, s, key, 0, -1), data) {
			t.Fatal("read differs")
		}
	})

	t.Run("errors carry no body", func(t *testing.T) {
		if h.Failing == nil {
			t.Skip("not applicable")
		}
		s, forbidden := h.Failing(t)
		key := Key([]byte("x"))
		calls := map[string]error{}
		calls["put"] = s.Put(ctx, key, strings.NewReader("x"), 1)
		_, calls["get"] = s.Get(ctx, key, 0, -1)
		_, calls["stat"] = s.Stat(ctx, key)
		calls["delete"] = s.Delete(ctx, key)
		calls["list"] = s.List(ctx, "streams/", func(string, int64, time.Time) error { return nil })
		for name, err := range calls {
			if err == nil {
				t.Errorf("%s succeeded against a failing server", name)
				continue
			}
			leaked(t, name, err, append(forbidden, Secret, key)...)
		}
	})

	t.Run("a stalled server fails in time, without leaks", func(t *testing.T) {
		if h.Stalling == nil {
			t.Skip("not applicable")
		}
		s, forbidden := h.Stalling(t)
		key := Key([]byte("x"))
		data := Data(1<<20, 9)
		calls := map[string]func() error{
			"put":  func() error { return s.Put(ctx, key, bytes.NewReader(data), int64(len(data))) },
			"stat": func() error { _, err := s.Stat(ctx, key); return err },
			"get": func() error {
				r, err := s.Get(ctx, key, 0, -1)
				if err != nil {
					return err
				}
				defer r.Close()
				_, err = io.ReadAll(r)
				return err
			},
			"delete": func() error { return s.Delete(ctx, key) },
		}
		for name, call := range calls {
			start := time.Now()
			err := call()
			if err == nil {
				t.Errorf("%s succeeded against a stalled server", name)
				continue
			}
			if d := time.Since(start); d > 15*time.Second {
				t.Errorf("%s took %s", name, d)
			}
			leaked(t, name, err, append(forbidden, key)...)
		}
		// a caller's deadline is reported as such, without leaks
		dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		_, err := s.Stat(dctx, key)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("stat past the caller's deadline: %v", err)
		}
		leaked(t, "stat with a deadline", err, append(forbidden, key)...)
	})

	t.Run("large put (multipart)", func(t *testing.T) {
		s := h.New(t)
		data := Data(17<<20+1, 6)
		key := Key(data)
		if err := s.Put(ctx, key, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
		if got := get(t, s, key, 16<<20-3, 10); !bytes.Equal(got, data[16<<20-3:16<<20+7]) {
			t.Fatal("range across the part boundary differs")
		}
		if !bytes.Equal(get(t, s, key, 0, -1), data) {
			t.Fatal("large object differs")
		}
		// a cancelled large put leaves nothing visible
		key2 := Key(append(data, 1))
		cctx, cancel := context.WithCancel(ctx)
		r := &cancelAt{data: data, at: 5 << 20, cancel: cancel}
		if err := s.Put(cctx, key2, r, int64(len(data))); err == nil {
			t.Fatal("a cancelled put succeeded")
		}
		if _, err := s.Stat(ctx, key2); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("a cancelled put is visible: %v", err)
		}
	})
}

// cancelAt cancels a context once a read reaches at.
type cancelAt struct {
	data   []byte
	at     int64
	cancel context.CancelFunc
}

func (c *cancelAt) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) >= c.at {
		c.cancel()
	}
	if off >= int64(len(c.data)) {
		return 0, io.EOF
	}
	n := copy(p, c.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func leaked(t *testing.T, name string, err error, forbidden ...string) {
	t.Helper()
	msg := fmt.Sprint(err)
	for _, f := range forbidden {
		if f != "" && strings.Contains(msg, f) {
			t.Errorf("%s error leaked %q: %s", name, f, msg)
		}
	}
}
