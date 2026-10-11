package blob

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Verification (spec 0018): every record of a domain against its object, quick (present and of the
// recorded length) or full (every chunk against its digest, through the reader serving uses). A
// mark means serving answers 503 until the body is committed again, so marks are written only from
// a full read, as a reader would write them, and never while the pass looks at the wrong store.

// Verify outcomes.
const (
	VerifyOK         = "ok"
	VerifyMarked     = "marked" // the object is intact, but the record is marked: served 503
	VerifyMissing    = "missing"
	VerifyShort      = "short"
	VerifyLong       = "long"       // longer than recorded: served all the same (a ranged read), never marked
	VerifyCorrupt    = "corrupt"    // a chunk differs from its digest
	VerifyRecord     = "record"     // the record itself is damaged
	VerifyUnreadable = "unreadable" // a transport error: never marked
)

// ErrImplausible stops a pass that finds most bodies missing: the store looks misconfigured (an
// unmounted directory, another bucket or prefix), and marking would take the domain offline.
var ErrImplausible = errors.New("blob: most bodies are missing: is this the domain's store? nothing more is marked")

// VerifyOptions configure a pass.
type VerifyOptions struct {
	Full        bool // read every chunk, not only the object's size
	Mark        bool // (Full) mark what a reader would: missing, short, corrupt
	Unmark      bool // (Full) clear the mark of a record whose object is intact
	Concurrency int  // records checked at once, 1..32
	// Problem is called for every record that is not ok, one at a time.
	Problem func(Problem)
}

// Problem is a record that is not ok.
type Problem struct {
	Domain, BodyHash, StreamHash, Outcome string
	Detail                                string // the chunk, the sizes, the error (one line)
	Marked                                bool   // marked by this pass
	Unmarked                              bool   // unmarked by this pass
	WasMarked                             bool   // marked before it
}

// VerifyResult counts a pass over a domain. Skipped records changed during the pass (collected, or
// committed again with another stream) and are not counted as problems.
type VerifyResult struct {
	Domain                                                                            string
	Full, Mark, Unmark                                                                bool
	Checked, OK, Skipped, MarkedOK, Missing, Short, Long, Corrupt, Record, Unreadable int
	Marked, Unmarked                                                                  int
	Bytes                                                                             int64 // read from the store (full)
}

func (r *VerifyResult) count(outcome string) {
	r.Checked++
	switch outcome {
	case VerifyOK:
		r.OK++
	case VerifyMarked:
		r.MarkedOK++
	case VerifyMissing:
		r.Missing++
	case VerifyShort:
		r.Short++
	case VerifyLong:
		r.Long++
	case VerifyCorrupt:
		r.Corrupt++
	case VerifyRecord:
		r.Record++
	case VerifyUnreadable:
		r.Unreadable++
	}
}

// Clean reports whether every record checked is ok (a record skipped changed during the pass).
func (r VerifyResult) Clean() bool { return r.Checked == r.OK+r.Skipped }

// implausible reports a pass where most records so far are missing.
func (r VerifyResult) implausible() bool {
	lost := r.Missing + r.Short
	return lost >= 3 && lost*2 > r.Checked-r.Skipped
}

var verifyPage = 500

// Verify checks every record of a domain. Marks are written per page, once the page is checked and
// the pass still looks at a plausible store. It stops on an error reading the records, on
// ErrImplausible, or when ctx ends; a record's own failure is a Problem, never an error.
func (s *Service) Verify(ctx context.Context, domain string, o VerifyOptions) (VerifyResult, error) {
	r := VerifyResult{Domain: domain, Full: o.Full, Mark: o.Mark, Unmark: o.Unmark}
	d, err := s.domain(domain)
	if err != nil {
		return r, err
	}
	switch {
	case o.Concurrency < 1 || o.Concurrency > 32:
		return r, fmt.Errorf("blob: concurrency is 1 to 32, not %d", o.Concurrency)
	case (o.Mark || o.Unmark) && !o.Full:
		return r, errors.New("blob: marking and unmarking need a full read")
	}
	after := ""
	for {
		rows, err := s.st.ListBlobs(ctx, domain, after, verifyPage)
		if err != nil {
			return r, err
		}
		if len(rows) == 0 {
			return r, nil
		}
		after = rows[len(rows)-1].BodyHash
		checked := make([]verified, len(rows))
		work := make(chan int)
		var wg sync.WaitGroup
		for range o.Concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range work {
					checked[i] = s.verifyOne(ctx, d, rows[i], o.Full)
				}
			}()
		}
	feed:
		for i := range rows {
			select {
			case work <- i:
			case <-ctx.Done():
				break feed
			}
		}
		close(work)
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return r, err // what was in flight is not counted: it saw the cancellation, not the store
		}
		for i := range checked {
			c := &checked[i]
			if c.skipped {
				r.Checked++
				r.Skipped++
				continue
			}
			r.count(c.Outcome)
			r.Bytes += c.read
		}
		if o.Mark && r.implausible() {
			return r, ErrImplausible
		}
		for i := range checked {
			c := &checked[i]
			if c.skipped {
				continue
			}
			if c.Outcome != VerifyOK {
				s.change(ctx, c, rows[i].Blob, o)
				if c.Marked {
					r.Marked++
				}
				if c.Unmarked {
					r.Unmarked++
				}
				if o.Problem != nil {
					o.Problem(c.Problem)
				}
			}
		}
	}
}

// verified is one record's check.
type verified struct {
	Problem
	read    int64 // bytes read
	skipped bool  // the record changed during the pass
}

// verifyOne checks one record: its object's size, then (full) every chunk through the reader serving
// uses, which tells a transport error from a missing or short object as serving does.
func (s *Service) verifyOne(ctx context.Context, d Domain, row store.BlobRow, full bool) verified {
	v := verified{Problem: Problem{Domain: row.Domain, BodyHash: row.BodyHash, StreamHash: row.StreamHash, Outcome: VerifyOK,
		WasMarked: !row.CorruptAt.IsZero()}}
	if row.Err != nil {
		v.Outcome, v.Detail = VerifyRecord, oneLine(row.Err.Error())
		return v
	}
	size, err := d.Store.Stat(ctx, StreamKey(row.StreamHash))
	switch {
	case errors.Is(err, ErrNotFound):
		v.Outcome = VerifyMissing
	case err != nil:
		v.Outcome, v.Detail = VerifyUnreadable, oneLine(err.Error())
	case size < row.StreamLen:
		v.Outcome, v.Detail = VerifyShort, fmt.Sprintf("%d of %d bytes", size, row.StreamLen)
	case size > row.StreamLen:
		v.Outcome, v.Detail = VerifyLong, fmt.Sprintf("%d bytes, %d recorded", size, row.StreamLen)
	}
	if full && v.Outcome != VerifyUnreadable {
		// the reader decides, as when serving: a missing or short object is confirmed by a read (a
		// ranged Get, then the size), and a long one is read to stream_len, its chunks deciding
		long := v.Outcome == VerifyLong
		v.Outcome = VerifyOK
		sr := s.newStream(ctx, d, row.Blob)
		sr.quiet = true
		chunks := (row.StreamLen + extfile.ChunkSize - 1) / extfile.ChunkSize
		for k := int64(0); k < chunks && v.Outcome == VerifyOK; k++ {
			b, err := sr.chunk(k)
			switch {
			case err == nil:
				v.read += int64(len(b))
			case errors.Is(err, ErrCorrupt) && sr.why == whyMissing:
				v.Outcome = VerifyMissing
			case errors.Is(err, ErrCorrupt) && sr.why == whyShort:
				v.Outcome, v.Detail = VerifyShort, fmt.Sprintf("at chunk %d", k)
			case errors.Is(err, ErrCorrupt):
				v.Outcome, v.Detail = VerifyCorrupt, fmt.Sprintf("chunk %d", k)
			default:
				v.Outcome, v.Detail = VerifyUnreadable, oneLine(err.Error())
			}
		}
		sr.closeBody()
		if v.Outcome == VerifyOK && long {
			v.Outcome = VerifyLong
		}
	}
	if v.Outcome == VerifyOK && v.WasMarked {
		v.Outcome = VerifyMarked
	}
	if v.Outcome != VerifyOK && v.Outcome != VerifyRecord && ctx.Err() == nil {
		// a problem is reported only if the record is still the one read: the collector may have
		// removed it, or a commit replaced its stream, since its page was read
		now, err := s.st.GetBlob(ctx, row.Domain, row.BodyHash)
		if errors.Is(err, store.ErrNotFound) || err == nil && (now.StreamHash != row.StreamHash || !now.CommittedAt.Equal(row.CommittedAt)) {
			v.skipped = true
		}
	}
	return v
}

// change marks or unmarks a record after its page's check.
func (s *Service) change(ctx context.Context, v *verified, rec store.Blob, o VerifyOptions) {
	var err error
	switch {
	case o.Mark && !v.WasMarked && (v.Outcome == VerifyMissing || v.Outcome == VerifyShort || v.Outcome == VerifyCorrupt):
		v.Marked, err = s.st.MarkBlobCorrupt(ctx, rec)
	case o.Unmark && v.Outcome == VerifyMarked:
		v.Unmarked, err = s.st.UnmarkBlob(ctx, rec)
	default:
		return
	}
	s.forget(rec.Domain, rec.BodyHash)
	if err != nil {
		v.Detail = strings.TrimSpace(v.Detail + " (the record could not be changed: " + oneLine(err.Error()) + ")")
	}
}

// oneLine keeps an error on one line of the command's tab-separated output.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
