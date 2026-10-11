package blob

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Verification (spec 0018): every record of a domain against its object, quick (present and of the
// recorded length) or full (every chunk against its digest), through the reader serving uses.

// Verify outcomes.
const (
	VerifyOK         = "ok"
	VerifyMissing    = "missing"
	VerifyShort      = "short"
	VerifyLong       = "long"
	VerifyCorrupt    = "corrupt"
	VerifyRecord     = "record"     // the record itself is damaged
	VerifyUnreadable = "unreadable" // a transport error: never marked
)

// VerifyOptions configure a pass.
type VerifyOptions struct {
	Full        bool // read every chunk, not only the object's size
	Mark        bool // mark what is missing, short, long or corrupt, as a reader would
	Concurrency int  // records checked at once (1..32; 0: 4)
	// Problem is called for every record that is not ok, from the pass's goroutines one at a time.
	Problem func(Problem)
}

// Problem is a record that is not ok.
type Problem struct {
	Domain, BodyHash, StreamHash, Outcome string
	Detail                                string // the chunk, the sizes, the error
	Marked                                bool   // marked by this pass
	WasMarked                             bool   // already marked before it
}

// VerifyResult counts a pass over a domain.
type VerifyResult struct {
	Domain                                                                 string
	Full, Mark                                                             bool
	Checked, OK, Missing, Short, Long, Corrupt, Record, Unreadable, Marked int
	Bytes                                                                  int64 // read from the store (full)
}

func (r *VerifyResult) count(outcome string) {
	r.Checked++
	switch outcome {
	case VerifyOK:
		r.OK++
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

// Clean reports whether every record checked was ok.
func (r VerifyResult) Clean() bool { return r.Checked == r.OK }

const verifyPage = 500

// Verify checks every record of a domain. It stops on a store or database error reading the records,
// or when ctx ends; a record's own failure is a Problem, never an error.
func (s *Service) Verify(ctx context.Context, domain string, o VerifyOptions) (VerifyResult, error) {
	r := VerifyResult{Domain: domain, Full: o.Full, Mark: o.Mark}
	d, err := s.domain(domain)
	if err != nil {
		return r, err
	}
	n := o.Concurrency
	switch {
	case n == 0:
		n = 4
	case n < 1 || n > 32:
		return r, fmt.Errorf("blob: concurrency is 1 to 32, not %d", n)
	}
	var mu sync.Mutex // r and o.Problem
	after := ""
	for {
		rows, err := s.st.ListBlobs(ctx, domain, after, verifyPage)
		if err != nil {
			return r, err
		}
		if len(rows) == 0 {
			return r, ctx.Err()
		}
		after = rows[len(rows)-1].BodyHash
		work := make(chan store.BlobRow)
		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for row := range work {
					p, read := s.verifyOne(ctx, d, row, o)
					mu.Lock()
					r.count(p.Outcome)
					r.Bytes += read
					if p.Marked {
						r.Marked++
					}
					if p.Outcome != VerifyOK && o.Problem != nil {
						o.Problem(p)
					}
					mu.Unlock()
				}
			}()
		}
	feed:
		for _, row := range rows {
			select {
			case work <- row:
			case <-ctx.Done():
				break feed
			}
		}
		close(work)
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return r, err
		}
	}
}

// verifyOne checks one record and marks it when asked; it answers its outcome and the bytes read.
func (s *Service) verifyOne(ctx context.Context, d Domain, row store.BlobRow, o VerifyOptions) (Problem, int64) {
	p := Problem{Domain: row.Domain, BodyHash: row.BodyHash, StreamHash: row.StreamHash, Outcome: VerifyOK, WasMarked: !row.CorruptAt.IsZero()}
	if row.Err != nil {
		p.Outcome, p.Detail = VerifyRecord, row.Err.Error()
		return p, 0
	}
	size, err := d.Store.Stat(ctx, StreamKey(row.StreamHash))
	switch {
	case errors.Is(err, ErrNotFound):
		p.Outcome = VerifyMissing
	case err != nil:
		p.Outcome, p.Detail = VerifyUnreadable, err.Error()
		return p, 0
	case size < row.StreamLen:
		p.Outcome, p.Detail = VerifyShort, fmt.Sprintf("%d of %d bytes", size, row.StreamLen)
	case size > row.StreamLen:
		p.Outcome, p.Detail = VerifyLong, fmt.Sprintf("%d bytes, %d recorded", size, row.StreamLen)
	}
	var read int64
	if p.Outcome == VerifyOK && o.Full {
		sr := s.newStream(ctx, d, row.Blob)
		sr.quiet = true
		chunks := (row.StreamLen + extfile.ChunkSize - 1) / extfile.ChunkSize
		for k := int64(0); k < chunks; k++ {
			b, err := sr.chunk(k)
			if err != nil {
				switch {
				case errors.Is(err, ErrCorrupt) && sr.why == "missing":
					p.Outcome = VerifyMissing
				case errors.Is(err, ErrCorrupt) && sr.why == "short":
					p.Outcome, p.Detail = VerifyShort, fmt.Sprintf("at chunk %d", k)
				case errors.Is(err, ErrCorrupt):
					p.Outcome, p.Detail = VerifyCorrupt, fmt.Sprintf("chunk %d", k)
				default:
					p.Outcome, p.Detail = VerifyUnreadable, err.Error()
				}
				break
			}
			read += int64(len(b))
		}
		sr.closeBody()
	}
	if p.Outcome != VerifyOK && p.Outcome != VerifyUnreadable && o.Mark && !p.WasMarked {
		s.forget(row.Domain, row.BodyHash)
		marked, err := s.st.MarkBlobCorrupt(ctx, row.Blob)
		if err != nil {
			p.Detail += " (marking failed: " + err.Error() + ")"
		}
		p.Marked = marked
	}
	return p, read
}
