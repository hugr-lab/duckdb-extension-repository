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
	case errors.Is(err, errBodyNotSigned):
		problem(w, http.StatusBadRequest, typeInvalid, "the body is not the signed one: its SHA-256 is not the statement's")
	case errors.Is(err, blob.ErrRead) && errors.Is(err, os.ErrDeadlineExceeded):
		problem(w, http.StatusRequestTimeout, typeInvalid, "the upload was slower than this server accepts")
	case errors.Is(err, blob.ErrRead):
		problem(w, http.StatusBadRequest, typeInvalid, "the upload ended before its Content-Length")
	case errors.Is(err, blob.ErrBusy):
		w.Header().Set("Retry-After", "5")
		problem(w, http.StatusTooManyRequests, typeIngestBusy, "every ingest slot is taken; try again")
	case errors.Is(err, blob.ErrTooLarge), errors.Is(err, extfile.ErrTooLarge):
		problem(w, http.StatusRequestEntityTooLarge, typeTooLarge, "the file is larger than this server accepts")
	case errors.Is(err, extfile.ErrMalformed):
		problem(w, http.StatusBadRequest, typeInvalid, "the file is not a DuckDB extension: "+publicMessage(err))
	case errors.Is(err, release.ErrLicenseKeys):
		problem(w, http.StatusBadRequest, typeInvalid, "the file's licence key block is malformed (license_keys): "+
			"a publisher marked as a source of licence keys publishes only builds whose keys kista reads (kista ext inspect)")
	case errors.Is(err, release.ErrOtherSource):
		// spec 0031: the caller may publish or promote here, so it learns the name's source
		problem(w, http.StatusConflict, typeSource, publicRefusal(err))
	case errors.Is(err, store.ErrSourceGone):
		// spec 0033 phase A2: the publisher, or the key that signed the request, was removed while the
		// upload was in flight; it fails as a revoked key's request does
		unauthorized(w)
	case errors.Is(err, release.ErrHeld):
		// spec 0033 phase A2: a held entry, or a publisher with a hold on a channel's route
		problem(w, http.StatusConflict, typeHeld, publicRefusal(err))
	case errors.Is(err, release.ErrBlocked):
		problem(w, http.StatusConflict, typeConflict, "the body is blocked in this tenant")
	case errors.Is(err, store.ErrCatalogueFull):
		// spec 0037: no Retry-After: it clears when the source's builds are placed, purged or trimmed
		problem(w, http.StatusTooManyRequests, typeCatalogueFull, publicRefusal(err))
	case errors.Is(err, store.ErrTenantLimit): // spec 0038
		limitProblem(w, err)
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
		problem(w, http.StatusTooManyRequests, typeUploadSlots, "too many uploads at once; try again")
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

var publishParams = map[string]bool{"version": true, "platform": true, "visibility": true, "current": true, "type": true}

// upload is a publication's request: what its parameters say, and its body read at the server's
// least rate, holding an upload slot until done.
type upload struct {
	version, platform   string
	private, notCurrent bool
	body                io.Reader
	done                func()
}

// readUpload checks a publication's parameters and headers and takes an upload slot, or answers.
// current is whether the route takes "current" (a channel's).
func (h *Handler) readUpload(w http.ResponseWriter, r *http.Request, c caller, current bool) (upload, bool) {
	q := r.URL.Query()
	for k, vs := range q {
		if !publishParams[k] || k == "current" && !current || len(vs) != 1 {
			msg := "the parameters are version, platform, visibility, current and type, once each"
			if !current {
				msg = "the parameters are version, platform, visibility and type, once each"
			}
			problem(w, http.StatusBadRequest, typeInvalid, msg)
			return upload{}, false
		}
	}
	u := upload{version: q.Get("version"), platform: q.Get("platform"), private: true} // a forgotten visibility never publishes to everyone
	if u.version == "" || u.platform == "" {
		problem(w, http.StatusBadRequest, typeInvalid, "version and platform are required: what the file's footer says")
		return upload{}, false
	}
	switch q.Get("visibility") {
	case "", store.Private:
	case store.Public:
		u.private = false
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "visibility is public or private")
		return upload{}, false
	}
	switch q.Get("current") {
	case "", "true":
	case "false":
		u.notCurrent = true
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "current is true or false")
		return upload{}, false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/octet-stream" {
		problem(w, http.StatusUnsupportedMediaType, typeMediaType, "the body is the extension file, application/octet-stream")
		return upload{}, false
	}
	if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
		problem(w, http.StatusLengthRequired, typeInvalid, "Content-Length is required")
		return upload{}, false
	}
	if r.ContentLength > h.o.MaxBody+extfile.SignatureSize {
		problem(w, http.StatusRequestEntityTooLarge, typeTooLarge, "the file is larger than this server accepts")
		return upload{}, false
	}
	a, _ := c.actor()
	done, ok := h.slot(w, a.String(), c.tenant.ID)
	if !ok {
		return upload{}, false
	}
	now := time.Now()
	// the whole upload is due by its length at the least rate, plus a minute
	u.body = &rateReader{r: io.LimitReader(r.Body, r.ContentLength), rc: http.NewResponseController(w), start: now,
		rate: float64(h.o.MinRate), end: now.Add(time.Minute + time.Duration(float64(r.ContentLength)/float64(h.o.MinRate)*float64(time.Second)))}
	u.done = done
	return u, true
}

func (h *Handler) publishRelease(w http.ResponseWriter, r *http.Request, c caller, p params) {
	u, ok := h.readUpload(w, r, c, true)
	if !ok {
		return
	}
	defer u.done()
	a, _ := c.actor()
	rel, existed, err := h.o.Releases.Add(r.Context(), a, p["t"], p["c"], u.body, release.AddOptions{
		Name: p["ext"], Private: u.private, NotCurrent: u.notCurrent, NoWait: true,
		Publish: &release.Publication{Version: u.version, Platform: u.platform, Provenance: provenance(a, c), KeySources: keySources(c),
			Statement: c.statement(), Signature: c.signature(), KeyID: keyID(c)},
	})
	if err != nil {
		h.publishErr(w, err, false)
		return
	}
	h.answerReleases(w, r, c, p, []store.Release{rel}, existed, true)
}

// keyID is the id of the publisher's key that signed the request, "" for another caller.
func keyID(c caller) string {
	if c.pub == nil || len(c.pub.Publishers) == 0 {
		return ""
	}
	return c.pub.Publishers[0].KeyID
}

// keySources are the ids of the caller's matched publishers marked as sources of licence keys
// (spec 0021).
func keySources(c caller) []string {
	if c.pub == nil {
		return nil
	}
	var out []string
	for _, m := range c.pub.Publishers {
		if m.Publisher.LicenseKeys {
			out = append(out, m.Publisher.ID)
		}
	}
	return out
}

// provenance of a publication: the actor, and a publisher's credential (spec 0008, 0023).
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
	if !body(w, r, &in, maxBody) {
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
	if c.pub != nil { // a publisher's promotion records its credential and signed request too
		o.Provenance, o.Statement, o.Signature = c.pub.Provenance(a.String()), c.statement(), c.signature()
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
	typ, provided, err := h.channelProvided(r.Context(), c, p)
	if err != nil {
		h.fail(w, err)
		return
	}
	src, err := h.sourcesFor(r.Context(), c.tenant.ID, channelIDOf(all))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := []releaseJSON{}
	for _, rel := range rels {
		if x, ok := byID[rel.ID]; ok {
			out = append(out, releaseView(typ, x, c, full, provided, src))
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
	return h.o.Authz.Allow(r.Context(), a, authz.VerbAdmin, authz.Resource{Tenant: p["t"], Channel: p["c"], Package: p["ext"]}) == nil
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
	if !body(w, r, &in, maxBody) {
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

// UploadsActive is how many uploads this server is receiving (metrics).
func (h *Handler) UploadsActive() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return int64(h.uploads["all"])
}
