// Package fs is the local-directory object store (spec 0005): every access goes through os.Root,
// objects are written to tmp/, fsynced, renamed into place and the directory fsynced.
package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
)

// Store is a directory store.
type Store struct {
	root *os.Root
	dir  string
}

// Open opens (creating with mode 0700) the root directory.
func Open(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("blob/fs: the root must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("blob/fs: %w", err)
	}
	// the identity of the store is the real directory, so two spellings of it are one store
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("blob/fs: %w", err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("blob/fs: %w", err)
	}
	for _, d := range []string{"streams", "tmp"} {
		if err := r.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			_ = r.Close()
			return nil, fmt.Errorf("blob/fs: %w", err)
		}
	}
	return &Store{root: r, dir: filepath.Clean(dir)}, nil
}

// Close releases the root.
func (s *Store) Close() error { return s.root.Close() }

// ID implements blob.Store.
func (s *Store) ID() string { return "fs:" + s.dir }

// Anonymous implements blob.Store: a local directory has no anonymous access path.
func (s *Store) Anonymous(context.Context, string) (bool, bool) { return false, true }

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	if err := blob.ValidKey(key); err != nil {
		return err
	}
	tmp := "tmp/" + uuid.NewString()
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("blob/fs: %w", err)
	}
	done := false
	defer func() {
		if !done {
			f.Close()
			_ = s.root.Remove(tmp)
		}
	}()
	if _, err := io.Copy(f, io.NewSectionReader(r, 0, size)); err != nil {
		return fmt.Errorf("blob/fs: writing: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("blob/fs: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("blob/fs: %w", err)
	}
	if err := s.root.Rename(tmp, key); err != nil {
		return fmt.Errorf("blob/fs: %w", err)
	}
	done = true
	return s.syncDir(path.Dir(key))
}

func (s *Store) syncDir(dir string) error {
	d, err := s.root.Open(dir)
	if err != nil {
		return fmt.Errorf("blob/fs: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("blob/fs: %w", err)
	}
	return nil
}

type section struct {
	*io.SectionReader
	f *os.File
}

func (s section) Close() error { return s.f.Close() }

// Get implements blob.Store.
func (s *Store) Get(_ context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if err := blob.ValidKey(key); err != nil {
		return nil, err
	}
	if off < 0 {
		return nil, errors.New("blob/fs: negative offset")
	}
	f, err := s.root.Open(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, blob.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("blob/fs: %w", err)
	}
	if n < 0 {
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("blob/fs: %w", err)
		}
		n = fi.Size() - off
	}
	return section{io.NewSectionReader(f, off, n), f}, nil
}

// Stat implements blob.Store.
func (s *Store) Stat(_ context.Context, key string) (int64, error) {
	if err := blob.ValidKey(key); err != nil {
		return 0, err
	}
	fi, err := s.root.Stat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, blob.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("blob/fs: %w", err)
	}
	return fi.Size(), nil
}

// Delete implements blob.Store.
func (s *Store) Delete(_ context.Context, key string) error {
	if err := blob.ValidKey(key); err != nil {
		return err
	}
	if err := s.root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob/fs: %w", err)
	}
	return nil
}

// List implements blob.Store.
func (s *Store) List(ctx context.Context, prefix string, fn func(string, int64, time.Time) error) error {
	if err := blob.ValidListPrefix(prefix); err != nil {
		return err
	}
	d, err := s.root.Open(path.Clean(prefix))
	if err != nil {
		return fmt.Errorf("blob/fs: %w", err)
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("blob/fs: %w", err)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := prefix + e.Name()
		if blob.ValidKey(key) != nil || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if err := fn(key, info.Size(), info.ModTime()); err != nil {
			return err
		}
	}
	return nil
}
