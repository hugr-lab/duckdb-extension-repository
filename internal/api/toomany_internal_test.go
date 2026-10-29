package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0037: each 429 a refusal answers has its own problem type; a full catalogue carries no
// Retry-After (kista does not know when space frees up), the others say when to try again. The
// rate limiter's, the upload slots' and the issuer's are tested where they are reached
// (TestRateLimit, TestUploadConcurrency, TestManagementDetails).
func TestTooManyTypes(t *testing.T) {
	h := &Handler{o: Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	for _, c := range []struct {
		name, typ, retry string
		answer           func(w http.ResponseWriter)
	}{
		{"a publication without an ingest slot", "ingest-busy", "5", func(w http.ResponseWriter) {
			h.publishErr(w, fmt.Errorf("x: %w", blob.ErrBusy), false)
		}},
		{"a full catalogue", "catalogue-full", "", func(w http.ResponseWriter) {
			h.publishErr(w, fmt.Errorf("%w: 100 entries", store.ErrCatalogueFull), false)
		}},
		{"too many chunks of an upload", "chunks-busy", "1", func(w http.ResponseWriter) { h.uploadErr(w, upstream.ErrUploadBusy) }},
		{"a chunk without an ingest slot", "ingest-busy", "1", func(w http.ResponseWriter) { h.uploadErr(w, upstream.ErrIngestBusy) }},
	} {
		rec := httptest.NewRecorder()
		c.answer(rec)
		var p struct{ Type, Detail string }
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != http.StatusTooManyRequests ||
			p.Type != "urn:kista:problem:"+c.typ || p.Detail == "" || rec.Header().Get("Retry-After") != c.retry {
			t.Errorf("%s: %d %v %s", c.name, rec.Code, rec.Header(), rec.Body)
		}
	}
}
