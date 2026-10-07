package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/blobtest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
)

func open(t *testing.T) *fs.Store {
	t.Helper()
	s, err := fs.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestContract(t *testing.T) {
	blobtest.Run(t, blobtest.Harness{New: func(t *testing.T) blob.Store { return open(t) }})
}

func TestRelativeRootRefused(t *testing.T) {
	if _, err := fs.Open("blobs"); err == nil {
		t.Fatal("opened a relative root")
	}
}

func TestModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	s, err := fs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data := blobtest.Data(10, 1)
	key := blobtest.Key(data)
	if err := s.Put(context.Background(), key, strings.NewReader(string(data)), 10); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"": 0o700, "streams": 0o700, "tmp": 0o700, key: 0o600} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%q: mode %v, want %v", name, fi.Mode().Perm(), want)
		}
	}
	tmp, _ := os.ReadDir(filepath.Join(dir, "tmp"))
	if len(tmp) != 0 {
		t.Fatalf("tmp/ holds %d leftovers", len(tmp))
	}
}

// A symlink planted in the store cannot lead outside the root.
func TestSymlinkEscapeRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	s, err := fs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := blobtest.Key([]byte("planted"))
	if err := os.Symlink(outside, filepath.Join(dir, key)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), key, 0, -1); err == nil {
		t.Fatal("read through a symlink leaving the root")
	}
}
