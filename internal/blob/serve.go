package blob

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// File is one channel file served from a stored stream: the gzip name or the plain name.
type File struct {
	Size        int64
	ETag        string // quoted
	ContentType string
	newReader   func(ctx context.Context) Reader
}

// Reader reads a File. Every byte it returns was verified against the record; a mismatch returns
// ErrCorrupt (and marks the record), after which the response must be aborted, not ended.
type Reader interface {
	io.ReadSeekCloser
}

// NewReader returns a reader for one request; the caller closes it.
func (f *File) NewReader(ctx context.Context) Reader { return f.newReader(ctx) }

// etag is a strong validator: H(kind ‖ hashes ‖ signature). The gzip bytes depend on the stream (a
// re-commit with another compressor makes another stream), the plain bytes only on the body.
func etag(kind string, rec store.Blob, sig []byte) string {
	h := sha256.New()
	h.Write([]byte(kind))
	if kind == "gz" {
		b, _ := hex.DecodeString(rec.StreamHash)
		h.Write(b)
	}
	b, _ := hex.DecodeString(rec.BodyHash)
	h.Write(b)
	h.Write(sig)
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

func (s *Service) open(rec store.Blob, sig []byte) (Domain, error) {
	if len(sig) != extfile.SignatureSize {
		return Domain{}, fmt.Errorf("blob: signature is %d bytes, want %d", len(sig), extfile.SignatureSize)
	}
	if int64(len(rec.StreamChunks)) != (rec.StreamLen+extfile.ChunkSize-1)/extfile.ChunkSize*sha256.Size {
		return Domain{}, fmt.Errorf("%w: the record's chunk digests do not cover the stream", ErrCorrupt)
	}
	return s.domain(rec.Domain)
}

// OpenGzip serves rec (resolved by the caller through a Build it may see) with sig as a gzip file:
// header, stored stream, a final stored block with sig, trailer.
func (s *Service) OpenGzip(_ context.Context, rec store.Blob, sig []byte) (*File, error) {
	d, err := s.open(rec, sig)
	if err != nil {
		return nil, err
	}
	pre := extfile.Precompressed{BodyCRC32: rec.BodyCRC32, BodyLen: rec.BodyLen}
	head, tail := extfile.GzipHeader(), extfile.GzipTail(pre, sig)
	size := int64(len(head)) + rec.StreamLen + int64(len(tail))
	return &File{
		Size: size, ETag: etag("gz", rec, sig), ContentType: "application/gzip",
		newReader: func(ctx context.Context) Reader {
			return &gzReader{head: head, tail: tail, size: size, stream: s.newStream(ctx, d, rec)}
		},
	}, nil
}

// OpenPlain serves rec with sig as the plain file (body ‖ signature), inflating the verified stream.
func (s *Service) OpenPlain(_ context.Context, rec store.Blob, sig []byte) (*File, error) {
	d, err := s.open(rec, sig)
	if err != nil {
		return nil, err
	}
	size := rec.BodyLen + extfile.SignatureSize
	return &File{
		Size: size, ETag: etag("plain", rec, sig), ContentType: "application/octet-stream",
		newReader: func(ctx context.Context) Reader {
			return &plainReader{svc: s, ctx: ctx, d: d, rec: rec, sig: bytes.Clone(sig), size: size}
		},
	}, nil
}

// streamReader reads the stored stream from a position, one verified chunk at a time.
type streamReader struct {
	svc  *Service
	ctx  context.Context
	d    Domain
	rec  store.Blob
	body io.ReadCloser
	next int64 // stream offset of the next byte body returns

	buf      []byte // verified bytes of chunk bufChunk
	bufChunk int64
	err      error // sticky ErrCorrupt
}

func (s *Service) newStream(ctx context.Context, d Domain, rec store.Blob) *streamReader {
	return &streamReader{svc: s, ctx: ctx, d: d, rec: rec, bufChunk: -1}
}

// chunk returns the verified bytes of chunk k.
func (r *streamReader) chunk(k int64) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	if k == r.bufChunk {
		return r.buf, nil
	}
	start := k * extfile.ChunkSize
	n := min(int64(extfile.ChunkSize), r.rec.StreamLen-start)
	if r.body == nil || r.next != start {
		r.closeBody()
		body, err := r.d.Store.Get(r.ctx, StreamKey(r.rec.StreamHash), start, r.rec.StreamLen-start)
		if err != nil {
			return nil, r.failed(k, err)
		}
		r.body, r.next = body, start
	}
	if cap(r.buf) < extfile.ChunkSize {
		r.buf = make([]byte, extfile.ChunkSize)
	}
	buf := r.buf[:n]
	r.bufChunk = -1
	if _, err := io.ReadFull(r.body, buf); err != nil {
		r.closeBody()
		return nil, r.failed(k, err)
	}
	r.next += n
	if sha256.Sum256(buf) != [sha256.Size]byte(r.rec.StreamChunks[k*sha256.Size:(k+1)*sha256.Size]) {
		r.closeBody()
		return nil, r.corrupt(k, "digest")
	}
	r.buf, r.bufChunk = buf, k
	return buf, nil
}

// failed tells a missing or short object (corruption) from a transport error: a read that ends early
// is what a dropped connection looks like too, so the store is asked for the object's size.
func (r *streamReader) failed(k int64, err error) error {
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	size, serr := r.d.Store.Stat(r.ctx, StreamKey(r.rec.StreamHash))
	switch {
	case errors.Is(err, ErrNotFound) || errors.Is(serr, ErrNotFound):
		return r.corrupt(k, "missing")
	case serr == nil && size < r.rec.StreamLen:
		return r.corrupt(k, "short")
	}
	return fmt.Errorf("blob: reading the stream: %w", err)
}

func (r *streamReader) corrupt(k int64, why string) error {
	r.err = fmt.Errorf("%w: domain %s, chunk %d (%s)", ErrCorrupt, r.rec.Domain, k, why)
	r.svc.markCorrupt(r.ctx, r.rec, fmt.Sprintf("chunk %d: %s", k, why))
	return r.err
}

// markCorrupt logs a record whose object does not match, and records corrupt_at once per commit of
// the record (a record already marked is not written again, so a bad object does not turn every
// request into a database write; a mark never lands on a newer commit).
func (s *Service) markCorrupt(ctx context.Context, rec store.Blob, why string) {
	s.forget(rec.Domain, rec.BodyHash)
	s.log.Error("blob: a stored stream does not match its record", "domain", rec.Domain,
		"body_hash", rec.BodyHash, "stream_hash", rec.StreamHash, "reason", why)
	if !rec.CorruptAt.IsZero() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.st.MarkBlobCorrupt(ctx, rec); err != nil {
		s.log.Error("blob: recording corrupt_at failed", "error", err)
	}
}

// readAt copies verified stream bytes from off into p.
func (r *streamReader) readAt(p []byte, off int64) (int, error) {
	if off >= r.rec.StreamLen {
		return 0, io.EOF
	}
	k := off / extfile.ChunkSize
	c, err := r.chunk(k)
	if err != nil {
		return 0, err
	}
	return copy(p, c[off-k*extfile.ChunkSize:]), nil
}

func (r *streamReader) closeBody() {
	if r.body != nil {
		r.body.Close()
		r.body = nil
	}
}

// gzReader is header ‖ stream ‖ tail.
type gzReader struct {
	head, tail []byte
	size, pos  int64
	stream     *streamReader
}

func (g *gzReader) Read(p []byte) (int, error) {
	if g.stream.err != nil {
		return 0, g.stream.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	h, l := int64(len(g.head)), g.stream.rec.StreamLen
	var n int
	var err error
	switch {
	case g.pos >= g.size:
		return 0, io.EOF
	case g.pos < h:
		n = copy(p, g.head[g.pos:])
	case g.pos < h+l:
		n, err = g.stream.readAt(p, g.pos-h)
	default:
		n = copy(p, g.tail[g.pos-h-l:])
	}
	g.pos += int64(n)
	return n, err
}

func (g *gzReader) Seek(off int64, whence int) (int64, error) {
	p, err := seek(g.pos, g.size, off, whence)
	if err == nil {
		g.pos = p
	}
	return p, err
}

func (g *gzReader) Close() error { g.stream.closeBody(); return nil }

func seek(pos, size, off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += pos
	case io.SeekEnd:
		off += size
	default:
		return pos, errors.New("blob: invalid whence")
	}
	if off < 0 {
		return pos, errors.New("blob: negative position")
	}
	return off, nil
}

// plainReader inflates the verified stream and appends the signature. Seeking backwards restarts
// the inflation; the plain name is served whole (no ranges), so that is rare.
type plainReader struct {
	svc       *Service
	ctx       context.Context
	d         Domain
	rec       store.Blob
	sig       []byte
	size, pos int64

	stream  *streamReader
	sp      int64 // stream position fed to the inflater
	inflate io.ReadCloser
	out     int64 // bytes inflated so far
	checked bool  // the end of the stream was checked
	err     error // sticky ErrCorrupt
}

func (p *plainReader) Read(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	n, err := p.read(b)
	if errors.Is(err, ErrCorrupt) {
		if p.stream == nil || p.stream.err == nil {
			// verified bytes that do not inflate to the record: the record and the stream disagree
			p.svc.markCorrupt(p.ctx, p.rec, err.Error())
		}
		p.err = err
	}
	return n, err
}

func (p *plainReader) read(b []byte) (int, error) {
	if p.pos >= p.size {
		return 0, io.EOF
	}
	if p.inflate == nil || p.pos < p.out {
		p.restart()
	}
	if err := p.inflateTo(min(p.pos, p.rec.BodyLen)); err != nil {
		return 0, err
	}
	if p.pos >= p.rec.BodyLen {
		n := copy(b, p.sig[p.pos-p.rec.BodyLen:])
		p.pos += int64(n)
		return n, nil
	}
	b = b[:min(int64(len(b)), p.rec.BodyLen-p.pos)]
	n, err := p.inflate.Read(b)
	p.out += int64(n)
	p.pos += int64(n)
	if n == 0 && err != nil {
		return 0, p.inflateErr(err)
	}
	if p.out == p.rec.BodyLen && !p.checked {
		if err := p.checkEnd(); err != nil {
			return n, err
		}
		p.checked = true
	}
	return n, nil
}

// inflateTo inflates and discards up to body offset target; at the end of the body it checks that
// the stream ends there.
func (p *plainReader) inflateTo(target int64) error {
	for p.out < target {
		k, err := io.CopyN(io.Discard, p.inflate, target-p.out)
		p.out += k
		if err != nil {
			return p.inflateErr(err)
		}
	}
	if p.out == p.rec.BodyLen && !p.checked {
		if err := p.checkEnd(); err != nil {
			return err
		}
		p.checked = true
	}
	return nil
}

func (p *plainReader) restart() {
	if p.stream != nil {
		p.stream.closeBody()
	}
	p.stream = p.svc.newStream(p.ctx, p.d, p.rec)
	p.sp, p.out, p.checked = 0, 0, false
	p.inflate = flate.NewReader(&plainStream{p})
}

type plainStream struct{ p *plainReader }

func (s *plainStream) Read(b []byte) (int, error) {
	n, err := s.p.stream.readAt(b, s.p.sp)
	s.p.sp += int64(n)
	return n, err
}

// inflateErr maps an inflater error: a stream error from the verified bytes is the record's fault
// (it cannot happen for a stream Precompress checked), anything else is passed on.
func (p *plainReader) inflateErr(err error) error {
	if errors.Is(err, ErrCorrupt) || p.stream.err != nil {
		return p.stream.err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: the stream inflates to %d bytes, the record says %d", ErrCorrupt, p.out, p.rec.BodyLen)
	}
	var ce flate.CorruptInputError
	if errors.As(err, &ce) {
		return fmt.Errorf("%w: the stream does not inflate", ErrCorrupt)
	}
	return err
}

// checkEnd verifies that the inflater has nothing more: it must run out of input (no final block)
// exactly at the end of the stream.
func (p *plainReader) checkEnd() error {
	var one [1]byte
	n, err := p.inflate.Read(one[:])
	switch {
	case n != 0:
		return fmt.Errorf("%w: the stream inflates to more than the record's %d bytes", ErrCorrupt, p.rec.BodyLen)
	case errors.Is(err, io.ErrUnexpectedEOF) && p.sp == p.rec.StreamLen:
		return nil
	case p.stream.err != nil:
		return p.stream.err
	case err == nil || errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%w: the stream has bytes past the body", ErrCorrupt)
	case errors.Is(err, io.EOF):
		return fmt.Errorf("%w: the stream has a final block", ErrCorrupt)
	}
	return err
}

func (p *plainReader) Seek(off int64, whence int) (int64, error) {
	np, err := seek(p.pos, p.size, off, whence)
	if err == nil {
		p.pos = np
	}
	return np, err
}

func (p *plainReader) Close() error {
	if p.stream != nil {
		p.stream.closeBody()
	}
	return nil
}
