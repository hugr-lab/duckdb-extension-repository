package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
)

// breaking breaks each of its first reads after a few bytes, as a dropped connection would.
type breaking struct {
	blob.Store
	breaks int
	opens  []int64 // the offsets Get was asked for
}

type broken struct {
	io.Reader
	io.Closer
}

func (f *breaking) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	f.opens = append(f.opens, off)
	rc, err := f.Store.Get(ctx, key, off, n)
	if err != nil || f.breaks == 0 {
		return rc, err
	}
	f.breaks--
	return broken{io.MultiReader(io.LimitReader(rc, 1000), errReader{}), rc}, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// Spec 0014: an import object is read under imports/ only, and a broken read re-opens it at its
// offset, up to five times.
func TestOpenImport(t *testing.T) {
	blob.SetImportPause(t, time.Millisecond)
	dir := filepath.Join(t.TempDir(), "blobs")
	f := &breaking{Store: openFS(t, dir)}
	svc, err := blob.NewService(ctx, openStore(t), []blob.Domain{{Name: "default", Kind: "fs", Store: f}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 1 << 20, MaxIngests: 1, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	data := bytes.Repeat([]byte("kista bundle "), 1000)
	if err := os.MkdirAll(filepath.Join(dir, "imports"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "imports", "b-1.kbundle"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	f.breaks, f.opens = 3, nil // the service read its marker
	rc, size, err := svc.OpenImport(ctx, "default", "b-1.kbundle")
	if err != nil || size != int64(len(data)) {
		t.Fatal(size, err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data) || len(f.opens) != 4 || f.opens[1] != 1000 || f.opens[3] != 3000 {
		t.Fatalf("a read with breaks: %d bytes, %v, opens %v", len(got), err, f.opens)
	}
	f.breaks, f.opens = 10, nil
	rc, _, _ = svc.OpenImport(ctx, "default", "b-1.kbundle")
	if _, err := io.ReadAll(rc); err == nil || len(f.opens) != 6 {
		t.Errorf("more breaks than retries: %v, opens %v", err, f.opens)
	}
	rc.Close()
	for _, name := range []string{"../kista-domain.json", "", "B", "a/b", "x..y"} {
		if _, _, err := svc.OpenImport(ctx, "default", name); !errors.Is(err, blob.ErrKey) {
			t.Errorf("%q: %v", name, err)
		}
	}
	if _, _, err := svc.OpenImport(ctx, "default", "missing"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("a missing object: %v", err)
	}
	if _, _, err := svc.OpenImport(ctx, "nope", "b-1.kbundle"); !errors.Is(err, blob.ErrDomain) {
		t.Errorf("an unknown domain: %v", err)
	}
}
