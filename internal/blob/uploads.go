package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/google/uuid"
)

// Bundle uploads' chunks (spec 0014 phase 2): each chunk is spooled, its digest checked, and stored
// as an object of its own, uploads/<uuid>, a new one per attempt; kista writes, reads and deletes
// them, and the collector sweeps what no chunk row names.

// ErrDigest is a chunk whose bytes do not match the digest its request gave.
var ErrDigest = errors.New("blob: the chunk does not match its digest")

// PutChunk reads a chunk of exactly size bytes from r into the spool (taking an ingest slot, or
// ErrBusy when none is free), checks its SHA-256 against want (hex), and stores it as a new object.
// It answers the object's key.
func (s *Service) PutChunk(ctx context.Context, domain string, r io.Reader, size int64, want string) (string, error) {
	d, err := s.domain(domain)
	if err != nil {
		return "", err
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return "", ErrBusy
	}
	defer func() { <-s.sem }()
	name, f, err := s.create()
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
		_ = s.spool.Remove(name)
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(ctxReader{ctx, r}, size+1))
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return "", fmt.Errorf("blob: spool: %w", err)
		}
		return "", fmt.Errorf("%w: %w", ErrRead, err)
	}
	if n != size {
		return "", fmt.Errorf("%w: %d bytes, not %d", ErrRead, n, size)
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return "", ErrDigest
	}
	key := "uploads/" + uuid.NewString()
	if err := d.Store.Put(ctx, key, f, size); err != nil {
		return "", err
	}
	return key, nil
}

// OpenChunk reads a stored chunk.
func (s *Service) OpenChunk(ctx context.Context, domain, key string) (io.ReadCloser, error) {
	d, err := s.domain(domain)
	if err != nil {
		return nil, err
	}
	if !uploadKey.MatchString(key) {
		return nil, fmt.Errorf("%w: %q", ErrKey, key)
	}
	return d.Store.Get(ctx, key, 0, -1)
}

// DeleteChunk deletes a stored chunk; a missing one is no error.
func (s *Service) DeleteChunk(ctx context.Context, domain, key string) error {
	d, err := s.domain(domain)
	if err != nil {
		return err
	}
	if err := d.Store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// bundleUploads is the collector's step for uploads: it expires idle and failed uploads (by
// conditional updates, before any object goes), deletes the chunks of uploads imported, aborted or
// expired, then their rows, and deletes the objects under uploads/ older than the cutoff that no
// chunk row names (a crash's, a deleted tenant's).
func (c *Collector) bundleUploads(ctx context.Context, d Domain, cutoff time.Time, r *Result) error {
	st := c.Service.st
	if !r.DryRun && c.UploadTTL > 0 {
		if _, err := st.ExpireUploads(ctx, d.Name, st.Now(), c.UploadTTL, c.UploadMaxAge); err != nil {
			return err
		}
	}
	for {
		done, err := st.DoneChunks(ctx, d.Name, 500)
		if err != nil || len(done) == 0 {
			return c.orphanChunks(ctx, d, cutoff, r, err)
		}
		for _, ch := range done {
			r.Chunks++
			r.Bytes += ch.Size
			if r.DryRun {
				continue
			}
			if err := d.Store.Delete(ctx, ch.Key); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if err := st.DeleteChunkRow(ctx, ch.UploadID, ch.N); err != nil {
				return err
			}
		}
		if r.DryRun || len(done) < 500 {
			return c.orphanChunks(ctx, d, cutoff, r, nil)
		}
	}
}

func (c *Collector) orphanChunks(ctx context.Context, d Domain, cutoff time.Time, r *Result, err error) error {
	if err != nil {
		return err
	}
	known, err := c.Service.st.ChunkKeys(ctx)
	if err != nil {
		return err
	}
	return d.Store.List(ctx, "uploads/", func(key string, size int64, modified time.Time) error {
		if !modified.Before(cutoff) || known[key] {
			return nil // a chunk whose row may be committing now, or a live one
		}
		r.Chunks++
		r.Bytes += size
		if r.DryRun {
			return nil
		}
		if err := d.Store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		return nil
	})
}
