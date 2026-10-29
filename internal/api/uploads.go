package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Bundles uploaded over the API (spec 0014 phase 2): a server administrator's sessions of chunks,
// and every import's record.

type uploadJSON struct {
	ID          string                 `json:"id"`
	Size        int64                  `json:"size"`
	SHA256      string                 `json:"sha256,omitempty"`
	ChunkSize   int64                  `json:"chunk_size"`
	Chunks      int64                  `json:"chunks"`
	Received    []int64                `json:"received,omitempty"`
	State       string                 `json:"state"`
	Options     upstream.UploadOptions `json:"options"`
	ImportID    string                 `json:"import_id,omitempty"` // once its record exists; a single upload's
	InputSHA256 string                 `json:"input_sha256,omitempty"`
	Attempts    int                    `json:"attempts"`
	Error       string                 `json:"error,omitempty"`
	CreatedBy   string                 `json:"created_by"`
	CompletedBy string                 `json:"completed_by,omitempty"`
	CreatedAt   string                 `json:"created_at"`
	UpdatedAt   string                 `json:"updated_at"`
	ExpiresAt   string                 `json:"expires_at,omitempty"`
}

// uploadOf is an upload's JSON; one shown alone has its chunks and (once its record exists) its
// import, a listed one neither: no lookup per upload.
func (h *Handler) uploadOf(r *http.Request, u store.BundleUpload, chunks []store.UploadChunk, alone bool, c caller) uploadJSON {
	o := uploadJSON{ID: u.ID, Size: u.Size, SHA256: u.SHA256, ChunkSize: u.ChunkSize, Chunks: u.Chunks(), State: u.State,
		InputSHA256: u.InputSHA256, Attempts: u.Attempts, Error: u.Error, CreatedBy: createdBy(u.CreatedBy, c),
		CompletedBy: createdBy(u.CompletedBy, c), CreatedAt: timeOf(u.CreatedAt), UpdatedAt: timeOf(u.UpdatedAt),
		ExpiresAt: timeOf(h.o.Upstreams.UploadExpiresAt(u))}
	_ = json.Unmarshal([]byte(u.Options), &o.Options)
	for _, ch := range chunks {
		o.Received = append(o.Received, ch.N)
	}
	if u.ImportID != "" && alone {
		if _, err := h.o.Upstreams.GetImport(r.Context(), mustActor(c), c.tenant.Name, u.ImportID); err == nil {
			o.ImportID = u.ImportID
		}
	}
	return o
}

func mustActor(c caller) authz.Actor { a, _ := c.actor(); return a }

// uploadErr maps an upload's refusals: a state or a limit is 409, a busy replica 429 (spec 0037: its
// own type for too many chunks of the upload, and for no ingest slot).
func (h *Handler) uploadErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, upstream.ErrUploadBusy):
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusTooManyRequests, typeChunksBusy, "too many chunks at once: send it again")
	case errors.Is(err, upstream.ErrIngestBusy):
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusTooManyRequests, typeIngestBusy, "every ingest slot is taken; send it again")
	case errors.Is(err, upstream.ErrUploadChunk):
		notFound(w)
	case errors.Is(err, blob.ErrDigest):
		problem(w, http.StatusBadRequest, typeInvalid, "the chunk does not match its Content-Digest")
	case errors.Is(err, blob.ErrRead):
		problem(w, http.StatusBadRequest, typeInvalid, "the chunk could not be read whole")
	case errors.Is(err, store.ErrUploadState), errors.Is(err, store.ErrConflict):
		problem(w, http.StatusConflict, typeConflict, strings.TrimPrefix(publicRefusal(err), "store: "))
	default:
		h.serviceErr(w, err, false)
	}
}

func (h *Handler) listUploads(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	us, err := h.o.Upstreams.ListUploads(r.Context(), a, p["t"], 100)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []uploadJSON{}
	for _, u := range us {
		out = append(out, h.uploadOf(r, u, nil, false, c))
	}
	reply(w, r, http.StatusOK, map[string]any{"uploads": out}, "", noStore)
}

func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Size      int64    `json:"size"`
		SHA256    string   `json:"sha256"`
		ChunkSize int64    `json:"chunk_size"`
		DryRun    bool     `json:"dry_run"`
		Upstreams []string `json:"upstreams"`
		Feeds     []string `json:"feeds"`
		AnyTenant bool     `json:"any_tenant"`
	}
	if !body(w, r, &in, maxBody) {
		return
	}
	a, _ := c.actor()
	u, err := h.o.Upstreams.CreateUpload(r.Context(), a, p["t"], in.Size, in.SHA256, in.ChunkSize,
		upstream.UploadOptions{DryRun: in.DryRun, Upstreams: in.Upstreams, Feeds: in.Feeds, AnyTenant: in.AnyTenant})
	if err != nil {
		h.uploadErr(w, err)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/bundle-uploads/"+esc(u.ID), h.uploadOf(r, u, nil, false, c), "")
}

func (h *Handler) getUpload(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	u, chunks, err := h.o.Upstreams.GetUpload(r.Context(), a, p["t"], p["id"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, h.uploadOf(r, u, chunks, true, c), "", noStore)
}

func (h *Handler) abortUpload(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Upstreams.AbortUpload(r.Context(), a, p["t"], p["id"]); err != nil {
		h.uploadErr(w, err)
		return
	}
	noContent(w)
}

func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	u, err := h.o.Upstreams.CompleteUpload(r.Context(), a, p["t"], p["id"])
	if err != nil {
		h.uploadErr(w, err)
		return
	}
	reply(w, r, http.StatusAccepted, h.uploadOf(r, u, nil, false, c), "", noStore)
}

// putChunk takes one chunk: exactly its bytes, with its RFC 9530 Content-Digest (sha-256), read at
// no less than the server's minimum rate.
func (h *Handler) putChunk(w http.ResponseWriter, r *http.Request, c caller, p params) {
	n, err := strconv.ParseInt(p["n"], 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != p["n"] {
		notFound(w)
		return
	}
	if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
		problem(w, http.StatusLengthRequired, typeInvalid, "Content-Length is required")
		return
	}
	digest := sha256Digest(r.Header.Get("Content-Digest"))
	a, _ := c.actor()
	now := time.Now()
	rd := &rateReader{r: r.Body, rc: http.NewResponseController(w), start: now, rate: float64(h.o.MinRate),
		end: now.Add(time.Minute + time.Duration(float64(r.ContentLength)/float64(h.o.MinRate)*float64(time.Second)))}
	if err := h.o.Upstreams.PutUploadChunk(r.Context(), a, p["t"], p["id"], n, rd, r.ContentLength, digest); err != nil {
		h.uploadErr(w, err)
		return
	}
	noContent(w)
}

// sha256Digest is an RFC 9530 Content-Digest's sha-256, as hex; "" when absent or malformed.
func sha256Digest(header string) string {
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "sha-256") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) < 2 || v[0] != ':' || v[len(v)-1] != ':' {
			return ""
		}
		b, err := base64.StdEncoding.DecodeString(v[1 : len(v)-1])
		if err != nil || len(b) != 32 {
			return ""
		}
		return hex.EncodeToString(b)
	}
	return ""
}

type importJSON struct {
	ID          string          `json:"id"`
	Manifest    string          `json:"manifest"`
	CreatedAt   string          `json:"manifest_created_at"`
	SHA256      string          `json:"manifest_sha256"`
	Exporter    json.RawMessage `json:"exporter,omitempty"`
	BundleKey   string          `json:"bundle_key"`
	InputSHA256 string          `json:"input_sha256,omitempty"`
	DryRun      bool            `json:"dry_run"`
	State       string          `json:"state"`
	Counts      json.RawMessage `json:"counts,omitempty"`
	StartedAt   string          `json:"started_at"`
	FinishedAt  string          `json:"finished_at,omitempty"`
	Actor       string          `json:"actor"`
}

func importOf(i store.BundleImport, c caller) importJSON {
	o := importJSON{ID: i.ID, Manifest: i.ManifestID, CreatedAt: timeOf(i.ManifestCreatedAt), SHA256: i.ManifestSHA256, BundleKey: i.BundleKey,
		InputSHA256: i.InputSHA256, DryRun: i.DryRun, State: i.State, StartedAt: timeOf(i.StartedAt), FinishedAt: timeOf(i.FinishedAt),
		Actor: createdBy(i.Actor, c)}
	if i.Interrupted {
		o.State = "failed (interrupted)"
	}
	if json.Valid([]byte(i.Exporter)) {
		o.Exporter = json.RawMessage(i.Exporter)
	}
	if json.Valid([]byte(i.Counts)) {
		o.Counts = json.RawMessage(i.Counts)
	}
	return o
}

func (h *Handler) listImports(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	is, err := h.o.Upstreams.Imports(r.Context(), a, p["t"], 100)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []importJSON{}
	for _, i := range is {
		out = append(out, importOf(i, c))
	}
	reply(w, r, http.StatusOK, map[string]any{"imports": out}, "", noStore)
}

func (h *Handler) getImport(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	i, err := h.o.Upstreams.GetImport(r.Context(), a, p["t"], p["id"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, importOf(i, c), "", noStore)
}
