package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// importRetries is how often a read of an import object re-opens it after a transient error.
const importRetries = 5

// OpenImport opens an object the operator uploaded under a domain's imports/ prefix (spec 0014)
// for reading front to back. A read error re-opens the object at the current offset, up to
// importRetries times, instead of failing a read of gigabytes. kista never writes or deletes it.
func (s *Service) OpenImport(ctx context.Context, domain, name string) (io.ReadCloser, int64, error) {
	d, err := s.domain(domain)
	if err != nil {
		return nil, 0, err
	}
	key, err := ImportKey(name)
	if err != nil {
		return nil, 0, err
	}
	size, err := d.Store.Stat(ctx, key)
	if err != nil {
		return nil, 0, fmt.Errorf("blob: domain %s: %s: %w", domain, key, err)
	}
	return &importReader{ctx: ctx, st: d.Store, key: key, size: size}, size, nil
}

// importReader reads an import object; Close may come from another goroutine while a Read waits
// (an import ending on a stall), so the open stream is guarded.
type importReader struct {
	mu      sync.Mutex
	closed  bool
	ctx     context.Context
	st      Store
	key     string
	size    int64
	off     int64
	rc      io.ReadCloser
	retries int
}

// importPause is the wait before a re-open (tests shorten it).
var importPause = 2 * time.Second

func (r *importReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	for {
		rc, err := r.open()
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrKey) || errors.Is(err, errClosed) || !r.retry() {
				return 0, err
			}
			continue
		}
		n, err := rc.Read(p)
		r.off += int64(n)
		switch {
		case err == nil:
			return n, nil
		case errors.Is(err, io.EOF) && r.off == r.size:
			return n, io.EOF
		}
		// a broken read, or an object ending early: re-opened at the offset
		r.drop(rc)
		if n > 0 {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		if !r.retry() {
			return 0, err
		}
	}
}

var errClosed = errors.New("blob: the import object is closed")

// open returns the open stream, opening it at the offset if needed.
func (r *importReader) open() (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errClosed
	}
	if r.rc == nil {
		rc, err := r.st.Get(r.ctx, r.key, r.off, r.size-r.off)
		if err != nil {
			return nil, err
		}
		r.rc = rc
	}
	return r.rc, nil
}

func (r *importReader) drop(rc io.ReadCloser) {
	r.mu.Lock()
	if r.rc == rc {
		r.rc = nil
	}
	r.mu.Unlock()
	_ = rc.Close()
}

func (r *importReader) retry() bool {
	if r.retries >= importRetries || r.ctx.Err() != nil {
		return false
	}
	r.retries++
	select {
	case <-r.ctx.Done():
		return false
	case <-time.After(importPause):
		return true
	}
}

func (r *importReader) Close() error {
	r.mu.Lock()
	rc := r.rc
	r.rc, r.closed = nil, true
	r.mu.Unlock()
	if rc != nil {
		return rc.Close()
	}
	return nil
}
