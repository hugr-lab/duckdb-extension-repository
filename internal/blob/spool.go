package blob

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

const spoolPrefix = "spool-"

// openSpool creates the spool directory (mode 0700) if needed and checks that it is a directory
// this process owns and nobody else can enter.
func openSpool(dir string) (*os.Root, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("blob: spool_dir must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("blob: spool_dir: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("blob: spool_dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, errors.New("blob: spool_dir is not a directory")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("blob: spool_dir has mode %v; it must be 0700", fi.Mode().Perm())
	}
	if err := ownedByMe(fi); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("blob: spool_dir: %w", err)
	}
	// what was opened is what was checked (not a directory swapped in between)
	if opened, err := r.Stat("."); err != nil || !os.SameFile(fi, opened) {
		_ = r.Close()
		return nil, errors.New("blob: spool_dir changed while it was opened")
	}
	return r, nil
}

// sweep removes the spool files a previous run left.
func (s *Service) sweep() {
	d, err := s.spool.Open(".")
	if err != nil {
		return
	}
	defer d.Close()
	names, _ := d.Readdirnames(-1)
	for _, n := range names {
		if !strings.HasPrefix(n, spoolPrefix) {
			continue
		}
		// another process (kista serve, a kista admin release add) may be spooling right now:
		// only files untouched for an hour are a previous run's leftovers
		if fi, err := s.spool.Stat(n); err == nil && time.Since(fi.ModTime()) > staleSpool {
			_ = s.spool.Remove(n)
		}
	}
}

// staleSpool is how long a spool file must be untouched before a startup sweep removes it.
const staleSpool = time.Hour

// ErrTooLarge is returned by Spool for a file over max_body (plus the signature).
var ErrTooLarge = errors.New("blob: the file exceeds max_body")

// Spool is an incoming file held on local disk until it is committed or dropped.
type Spool struct {
	svc    *Service
	domain Domain
	name   string
	f      *os.File
	file   *extfile.File
	once   sync.Once
}

func (s *Service) create() (string, *os.File, error) {
	name := spoolPrefix + uuid.NewString()
	f, err := s.spool.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("blob: spool: %w", err)
	}
	return name, f, nil
}

// ErrRead is an incoming file that could not be read whole: the client stopped, or was too slow.
var ErrRead = errors.New("blob: the file could not be read whole")

// ErrBusy is SpoolNoWait's answer when every ingest slot is taken.
var ErrBusy = errors.New("blob: every ingest slot is taken; try again")

// Spool reads a whole extension file (at most max_body + the signature) into the spool for domain,
// and parses it. The caller verifies File() and then calls Commit; it always calls Close.
func (s *Service) Spool(ctx context.Context, domain string, r io.Reader) (*Spool, error) {
	return s.spoolFile(ctx, domain, r, true)
}

// SpoolNoWait is Spool, failing with ErrBusy instead of waiting for an ingest slot (an upload holds
// its client's connection; spec 0008).
func (s *Service) SpoolNoWait(ctx context.Context, domain string, r io.Reader) (*Spool, error) {
	return s.spoolFile(ctx, domain, r, false)
}

func (s *Service) spoolFile(ctx context.Context, domain string, r io.Reader, wait bool) (*Spool, error) {
	d, err := s.domain(domain)
	if err != nil {
		return nil, err
	}
	if wait {
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		select {
		case s.sem <- struct{}{}:
		default:
			return nil, ErrBusy
		}
	}
	name, f, err := s.create()
	if err != nil {
		<-s.sem
		return nil, err
	}
	sp := &Spool{svc: s, domain: d, name: name, f: f}
	ok := false
	defer func() {
		if !ok {
			sp.Close()
		}
	}()
	limit := s.maxBody + extfile.SignatureSize
	n, err := io.Copy(sp.f, io.LimitReader(ctxReader{ctx, r}, limit+1))
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) { // the spool file, not the incoming reader
			return nil, fmt.Errorf("blob: spool: %w", err)
		}
		return nil, fmt.Errorf("%w: %w", ErrRead, err)
	}
	if n > limit {
		return nil, ErrTooLarge
	}
	sp.file, err = extfile.Open(sp.f, n, limit)
	if err != nil {
		return nil, err
	}
	ok = true
	return sp, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// File is the parsed spooled file: metadata, body hash and signature, for the caller to verify.
func (sp *Spool) File() *extfile.File { return sp.file }

// Commit stores the body: it precompresses it into a second spool file, claims the stream (spec
// 0016: the collector never deletes a claimed stream), uploads it to its content address (always,
// even when the domain already holds it), and records it, its claim checked. A claim lost (expired,
// or a commit slower than its deadline) is taken again and the stream uploaded again.
func (sp *Spool) Commit(ctx context.Context) (store.Blob, error) {
	if sp.f == nil {
		return store.Blob{}, errors.New("blob: the spool is closed")
	}
	s := sp.svc
	zname, zf, err := s.create()
	if err != nil {
		return store.Blob{}, err
	}
	defer func() {
		zf.Close()
		_ = s.spool.Remove(zname)
	}()
	pre, err := extfile.Precompress(sp.file.Body(), zf, s.maxBody)
	if err != nil {
		return store.Blob{}, fmt.Errorf("blob: precompressing: %w", err)
	}
	if pre.BodyHash != sp.file.Hash || pre.BodyLen != sp.file.BodySize {
		return store.Blob{}, errors.New("blob: the spooled body changed while it was committed")
	}
	if err := zf.Sync(); err != nil {
		return store.Blob{}, fmt.Errorf("blob: spool: %w", err)
	}
	rec := store.Blob{
		Domain: sp.domain.Name, BodyHash: pre.BodyHash.String(), StreamHash: hex.EncodeToString(pre.StreamHash[:]),
		BodyLen: pre.BodyLen, BodyCRC32: pre.BodyCRC32, StreamLen: pre.StreamLen,
		StreamChunks: make([]byte, 0, len(pre.StreamChunks)*32),
	}
	for _, c := range pre.StreamChunks {
		rec.StreamChunks = append(rec.StreamChunks, c[:]...)
	}
	for attempt := 0; ; attempt++ {
		claim := store.NewID()
		if err := s.claim(ctx, rec.Domain, rec.StreamHash, claim); err != nil {
			return store.Blob{}, err
		}
		if err := sp.domain.Store.Put(ctx, StreamKey(rec.StreamHash), zf, pre.StreamLen); err != nil {
			_ = s.st.ReleaseClaim(context.WithoutCancel(ctx), rec.Domain, rec.StreamHash, claim)
			return store.Blob{}, err
		}
		// two first commits of one body (other compressors) race on the insert: the loser retries
		r := rec
		err = s.st.CommitBlob(ctx, &r, claim)
		if errors.Is(err, store.ErrExists) {
			r = rec
			err = s.st.CommitBlob(ctx, &r, claim)
		}
		if errors.Is(err, store.ErrClaimLost) && attempt < 2 {
			continue // claim again, upload again
		}
		if err != nil {
			_ = s.st.ReleaseClaim(context.WithoutCancel(ctx), rec.Domain, rec.StreamHash, claim)
			return store.Blob{}, err
		}
		s.forget(r.Domain, r.BodyHash)
		return r, nil
	}
}

// IntakeDeadline bounds an intake from its commit to its release (spec 0016): a claim lasts as long.
const IntakeDeadline = time.Hour

// busyPoll is how often a commit waiting on a stream's delete tries again.
var busyPoll = 2 * time.Second

// claim claims a stream until the intake's deadline (its context's, at most IntakeDeadline), waiting
// while the collector deletes it.
func (s *Service) claim(ctx context.Context, domain, stream, id string) error {
	until := s.st.Now().Add(IntakeDeadline)
	if d, ok := ctx.Deadline(); ok && d.Before(until) {
		until = d
	}
	for {
		err := s.st.ClaimStream(ctx, domain, stream, id, until)
		if !errors.Is(err, store.ErrStreamBusy) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", err, ctx.Err())
		case <-time.After(busyPoll):
		}
	}
}

// Close removes the spool file and frees the ingest slot; it is safe to call more than once.
func (sp *Spool) Close() {
	sp.once.Do(func() {
		if sp.f != nil {
			sp.f.Close()
			_ = sp.svc.spool.Remove(sp.name)
			sp.f = nil
		}
		<-sp.svc.sem
	})
}
