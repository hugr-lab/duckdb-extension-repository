package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
		// spec 0038: a tenant limit, raisable or at its ceiling
		{"a tenant's bytes limit", "tenant-limit", "", func(w http.ResponseWriter) {
			h.publishErr(w, fmt.Errorf("x: %w", store.LimitRefused(store.LimitBytes, 1<<30)), false)
		}},
		{"a tenant at the channels' ceiling", "tenant-limit", "", func(w http.ResponseWriter) {
			h.serviceErr(w, store.LimitRefused(store.LimitChannels, store.MaxChannels), false)
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

// Spec 0038: tenant-limit names its limit, and says a server administrator can raise it unless it
// is at its ceiling.
func TestTenantLimitProblem(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{store.LimitRefused(store.LimitPublishers, 3), "a server administrator can raise it"},
		{store.LimitRefused(store.LimitPublishers, store.MaxPublishers), "the most publishers kista allows"},
	} {
		rec := httptest.NewRecorder()
		if !limitProblem(rec, c.err) {
			t.Fatal("not a limit's refusal")
		}
		var p struct{ Type, Detail, Limit string }
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Limit != "publishers" || !strings.Contains(p.Detail, c.want) {
			t.Errorf("%v: %s", c.err, rec.Body)
		}
	}
}
