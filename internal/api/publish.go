package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0008: publication, promotion and blocks.

// publishErr maps a publication's or promotion's errors.
func (h *Handler) publishErr(w http.ResponseWriter, err error, bodyRef bool) {
	switch {
	case errors.Is(err, blob.ErrRead) && errors.Is(err, os.ErrDeadlineExceeded):
		problem(w, http.StatusRequestTimeout, typeInvalid, "the upload was slower than this server accepts")
	case errors.Is(err, blob.ErrRead):
		problem(w, http.StatusBadRequest, typeInvalid, "the upload ended before its Content-Length")
	case errors.Is(err, blob.ErrBusy):
		w.Header().Set("Retry-After", "5")
		problem(w, http.StatusTooManyRequests, typeTooMany, "every upload slot is taken; try again")
	case errors.Is(err, blob.ErrTooLarge), errors.Is(err, extfile.ErrTooLarge):
		problem(w, http.StatusRequestEntityTooLarge, typeTooLarge, "the file is larger than this server accepts")
	case errors.Is(err, extfile.ErrMalformed):
		problem(w, http.StatusBadRequest, typeInvalid, "the file is not a DuckDB extension: "+publicMessage(err))
	case errors.Is(err, release.ErrBlocked):
		problem(w, http.StatusConflict, typeConflict, "the body is blocked in this tenant")
	default:
		h.channelErr(w, err, bodyRef)
	}
}

// slot takes an upload slot for the actor, the tenant and the server (uploads never take every
// ingest slot: the CLI and intake keep one), or answers 429.
func (h *Handler) slot(w http.ResponseWriter, actor, tenant string) (func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.uploads["a:"+actor] >= h.o.PublishPerActor || h.uploads["t:"+tenant] >= h.o.PublishPerTenant ||
		h.uploads["all"] >= h.o.PublishMax {
		w.Header().Set("Retry-After", "5")
		problem(w, http.StatusTooManyRequests, typeTooMany, "too many uploads at once; try again")
		return nil, false
	}
	h.uploads["a:"+actor]++
	h.uploads["t:"+tenant]++
	h.uploads["all"]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, k := range []string{"a:" + actor, "t:" + tenant, "all"} {
			if h.uploads[k]--; h.uploads[k] <= 0 {
				delete(h.uploads, k)
			}
		}
	}, true
}

// rateReader reads an upload at no less than a rate: before each read the connection's read
// deadline moves to when the bytes so far, plus a window, are due, and never past an absolute end.
type rateReader struct {
	r     io.Reader
	rc    *http.ResponseController
	start time.Time
	end   time.Time
	rate  float64 // bytes per second
	n     int64
	done  bool
}

const (
	uploadGrace  = 30 * time.Second
	uploadWindow = 64 << 10
)

func (rr *rateReader) Read(p []byte) (int, error) {
	if rr.done {
		return 0, io.EOF
	}
	due := rr.start.Add(uploadGrace + time.Duration(float64(rr.n+uploadWindow)/rr.rate*float64(time.Second)))
	if due.After(rr.end) {
		due = rr.end
	}
	_ = rr.rc.SetReadDeadline(due) // a writer without a connection keeps the server's timeouts
	n, err := rr.r.Read(p)
	rr.n += int64(n)
	if err != nil {
		// the body is read (or failed): the rate's deadline must not outlive it, or the server's
		// background read would time out and cancel the request's context
		rr.done = true
		_ = rr.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

var publishParams = map[string]bool{"version": true, "platform": true, "visibility": true, "current": true}

func (h *Handler) publishRelease(w http.ResponseWriter, r *http.Request, c caller, p params) {
	q := r.URL.Query()
	for k, vs := range q {
		if !publishParams[k] || len(vs) != 1 {
			problem(w, http.StatusBadRequest, typeInvalid, "the parameters are version, platform, visibility and current, once each")
			return
		}
	}
	version, platform := q.Get("version"), q.Get("platform")
	if version == "" || platform == "" {
		problem(w, http.StatusBadRequest, typeInvalid, "version and platform are required: what the file's footer says")
		return
	}
	private := true // a forgotten visibility never publishes to everyone
	switch q.Get("visibility") {
	case "", store.Private:
	case store.Public:
		private = false
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "visibility is public or private")
		return
	}
	notCurrent := false
	switch q.Get("current") {
	case "", "true":
	case "false":
		notCurrent = true
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "current is true or false")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/octet-stream" {
		problem(w, http.StatusUnsupportedMediaType, typeMediaType, "the body is the extension file, application/octet-stream")
		return
	}
	if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
		problem(w, http.StatusLengthRequired, typeInvalid, "Content-Length is required")
		return
	}
	if r.ContentLength > h.o.MaxBody+extfile.SignatureSize {
		problem(w, http.StatusRequestEntityTooLarge, typeTooLarge, "the file is larger than this server accepts")
		return
	}
	a, _ := c.actor()
	done, ok := h.slot(w, a.String(), c.tenant.ID)
	if !ok {
		return
	}
	defer done()
	now := time.Now()
	// the whole upload is due by its length at the least rate, plus a minute
	body := &rateReader{r: io.LimitReader(r.Body, r.ContentLength), rc: http.NewResponseController(w), start: now,
		rate: float64(h.o.MinRate), end: now.Add(time.Minute + time.Duration(float64(r.ContentLength)/float64(h.o.MinRate)*float64(time.Second)))}
	rel, existed, err := h.o.Releases.Add(r.Context(), a, p["t"], p["c"], body, release.AddOptions{
		Name: p["ext"], Private: private, NotCurrent: notCurrent, NoWait: true,
		Publish: &release.Publication{Version: version, Platform: platform, Provenance: provenance(a, c)},
	})
	if err != nil {
		h.publishErr(w, err, false)
		return
	}
	h.answerReleases(w, r, c, p, []store.Release{rel}, existed, true)
}

// provenance of a publication: the actor, and for trusted publishing the run (spec 0008).
func provenance(a authz.Actor, c caller) string {
	if c.pub != nil {
		return c.pub.Provenance(a.String())
	}
	b, _ := json.Marshal(map[string]string{"actor": a.String()})
	return string(b)
}

func (h *Handler) promoteRelease(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		From       string `json:"from_channel"`
		Release    string `json:"release"`
		Version    string `json:"version"`
		Visibility string `json:"visibility"`
		Current    *bool  `json:"current"`
	}
	if !body(w, r, &in) {
		return
	}
	o := release.PromoteOptions{From: in.From, Release: in.Release, Version: in.Version}
	switch in.Visibility {
	case "":
	case store.Private:
		o.Private = true
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "visibility can only be narrowed to private; a promotion keeps the source's")
		return
	}
	o.NotCurrent = in.Current != nil && !*in.Current
	a, _ := c.actor()
	if c.pub != nil { // a publisher's promotion records its run too
		o.Provenance = c.pub.Provenance(a.String())
	}
	rels, existed, err := h.o.Releases.Promote(r.Context(), a, p["t"], p["c"], p["ext"], o)
	if err != nil {
		h.publishErr(w, err, false) // a source the caller may not publish from is as missing: 404
		return
	}
	h.answerReleases(w, r, c, p, rels, existed, len(rels) == 1 && in.Release != "")
}

// answerReleases answers created (201) or existing (200) releases in the management shape; one
// release answers itself with its Location, several a list.
func (h *Handler) answerReleases(w http.ResponseWriter, r *http.Request, c caller, p params, rels []store.Release, existed, one bool) {
	a, _ := c.actor()
	all, err := h.o.Releases.List(r.Context(), a, p["t"], p["c"], p["ext"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	full := h.isAdmin(r, c, p)
	byID := map[string]store.Candidate{}
	for _, x := range all {
		byID[x.ID] = x
	}
	out := []releaseJSON{}
	for _, rel := range rels {
		if x, ok := byID[rel.ID]; ok {
			out = append(out, releaseView(x, c, full))
		}
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	if one && len(out) == 1 {
		loc := "/api/v1/tenants/" + esc(p["t"]) + "/channels/" + esc(p["c"]) + "/extensions/" + esc(p["ext"]) + "/releases/" + esc(out[0].ID)
		w.Header().Set("Location", loc)
		reply(w, r, status, out[0], versionTag(byID[out[0].ID].Version), noStore)
		return
	}
	reply(w, r, status, map[string]any{"releases": out}, "-", noStore)
}

// isAdmin reports whether the caller administers the path's extension (it then sees who changed
// releases; publishers do not).
func (h *Handler) isAdmin(r *http.Request, c caller, p params) bool {
	a, ok := c.actor()
	if !ok {
		return false
	}
	return h.o.Authz.Allow(r.Context(), a, authz.VerbAdmin, authz.Resource{Tenant: p["t"], Channel: p["c"], Extension: p["ext"]}) == nil
}

// --- blocks ---

type blockJSON struct {
	BodyHash  string `json:"body_hash"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
}

func blockOf(b store.Block, c caller) blockJSON {
	return blockJSON{BodyHash: b.BodyHash, Reason: b.Reason, CreatedAt: timeOf(b.CreatedAt), CreatedBy: createdBy(b.CreatedBy, c)}
}

func (h *Handler) listBlocks(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	bs, err := h.o.Releases.ListBlocks(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	bs, next := paged(bs, func(b store.Block) string { return b.BodyHash }, limit, after)
	out := []blockJSON{}
	for _, b := range bs {
		out = append(out, blockOf(b, c))
	}
	reply(w, r, http.StatusOK, list("blocks", out, next), "", noStore)
}

func (h *Handler) addBlock(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		BodyHash string `json:"body_hash"`
		Reason   string `json:"reason"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	b, existed, err := h.o.Releases.Block(r.Context(), a, p["t"], in.BodyHash, in.Reason)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	loc := "/api/v1/tenants/" + esc(p["t"]) + "/blocks/" + esc(b.BodyHash)
	if existed {
		w.Header().Set("Location", loc)
		reply(w, r, http.StatusOK, blockOf(b, c), "-", noStore)
		return
	}
	created(w, r, loc, blockOf(b, c), "-")
}

func (h *Handler) getBlock(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	b, err := h.o.Releases.GetBlock(r.Context(), a, p["t"], p["hash"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, blockOf(b, c), "", noStore)
}

func (h *Handler) removeBlock(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Releases.Unblock(r.Context(), a, p["t"], p["hash"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}
