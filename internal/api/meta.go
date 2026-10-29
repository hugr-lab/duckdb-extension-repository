package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/icon"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0036 phase 1: an entry's metadata, an icon and a summary.
//
//	PUT …/catalogue/{source}/extensions/{ext}/meta {"summary": …, "icon": {"content_type", "data"} | {"sha256"} | null}
//	GET …/catalogue/{source}/extensions/{ext}/meta
//	GET …/catalogue/{source}/extensions/{ext}/icon

// maxMetaBody is the metadata route's body: a 256 KiB icon is about 342 KiB in base64.
const maxMetaBody = 384 << 10

type iconJSON struct {
	SHA256 string `json:"sha256"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// iconOf is an entry's icon as answers show it, nil for none.
func iconOf(m store.EntryMeta) *iconJSON {
	if m.IconSHA256 == "" {
		return nil
	}
	return &iconJSON{SHA256: m.IconSHA256, Width: m.IconWidth, Height: m.IconHeight}
}

// summaryOf is an entry's summary as answers show it, nil for none.
func summaryOf(m store.EntryMeta) *string {
	if m.Summary == "" {
		return nil
	}
	return &m.Summary
}

type entryMetaJSON struct {
	PackageType string    `json:"package_type,omitempty"`
	Source      string    `json:"source"`
	Name        string    `json:"name"`
	Distributor string    `json:"distributor,omitempty"`
	Summary     *string   `json:"summary"`
	Icon        *iconJSON `json:"icon"`
	SetAt       string    `json:"set_at,omitempty"`
	// SignedAt is the mark a signed write must not be older than: the last signed write's signed-at,
	// or a token's write's time (to the microsecond)
	SignedAt string `json:"signed_at,omitempty"`
	// SetBy is who wrote it last: to tenant administrators, server administrators and distribution
	// admins only, never to a publisher
	SetBy string `json:"set_by,omitempty"`
}

func (h *Handler) answerMeta(w http.ResponseWriter, r *http.Request, c caller, v release.EntryMetaView) {
	out := entryMetaJSON{PackageType: shownType(v.PackageType), Source: v.Label, Name: v.Name, Distributor: v.Distributor,
		Summary: summaryOf(v.EntryMeta), Icon: iconOf(v.EntryMeta), SetAt: timeOf(v.SetAt)}
	if !v.SignedAt.IsZero() {
		out.SignedAt = v.SignedAt.UTC().Format(time.RFC3339Nano)
	}
	if c.pub == nil && v.SetBy != "" {
		out.SetBy = createdBy(v.SetBy, c)
	}
	reply(w, r, http.StatusOK, out, "", noStore)
}

// metaErr answers a metadata route's error. A write refused as stale, or failed after its icon was
// uploaded, is a refused or failed catalogue.meta event in the tenant (a captured request replayed,
// or storage spent for nothing); a conflict, an invalid write and a full catalogue are logged.
func (h *Handler) metaErr(w http.ResponseWriter, r *http.Request, c caller, p params, err error) {
	record := func(outcome, reason string) {
		if h.o.Events == nil || c.tenant.ID == "" {
			return
		}
		f := map[string]any{"source": p["source"], "name": p["ext"], "reason": reason}
		if c.signed != nil {
			f["key"] = c.signed.Fingerprint
		}
		if t := shownType(reqType(r)); t != "" {
			f["package_type"] = t
		}
		h.o.Events.Refusal(r.Context(), c.tenant.ID, c.eventActor(), "catalogue.meta", outcome, "ext:"+p["ext"], f)
	}
	logged := func(status int) {
		h.o.Log.Info("api: metadata write refused", "actor", c.logName(), "tenant", p["t"], "source", p["source"], "ext", p["ext"],
			"status", status, "reason", publicRefusal(err))
	}
	var uploaded *release.UploadedError
	if errors.As(err, &uploaded) {
		record(audit.Failed, "uploaded, then "+uploadedReason(uploaded.Err))
	}
	switch {
	case errors.Is(err, release.ErrStale):
		record(audit.Refused, "stale")
		problem(w, http.StatusConflict, typeStale, publicRefusal(err))
	case errors.Is(err, release.ErrIconChanged):
		logged(http.StatusConflict)
		problem(w, http.StatusConflict, typeConflict, publicRefusal(err))
	case errors.Is(err, store.ErrCatalogueFull):
		logged(http.StatusTooManyRequests)
		problem(w, http.StatusTooManyRequests, typeCatalogueFull, publicRefusal(err))
	case errors.Is(err, store.ErrSourceGone):
		// the publisher, or the key that signed the request, was removed while the write was in
		// flight: it fails as a revoked key's request does (spec 0033 phase A2)
		unauthorized(w)
	case errors.Is(err, store.ErrInvalid):
		logged(http.StatusBadRequest)
		h.serviceErr(w, err, false)
	default:
		h.serviceErr(w, err, false)
	}
}

// uploadedReason words why a write failed after its upload, without an internal error's text.
func uploadedReason(err error) string {
	switch {
	case errors.Is(err, release.ErrStale):
		return "stale"
	case errors.Is(err, release.ErrIconChanged):
		return "the icon changed"
	case errors.Is(err, store.ErrSourceGone):
		return "the key or the publisher removed"
	case errors.Is(err, authz.ErrDenied):
		return "refused"
	}
	return "failed"
}

func (h *Handler) entryMeta(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	v, err := h.o.Releases.EntryMeta(r.Context(), a, p["t"], reqType(r), p["source"], p["ext"])
	if err != nil {
		h.metaErr(w, r, c, p, err)
		return
	}
	h.answerMeta(w, r, c, v)
}

// metaWritesPerActor is the rate of an actor's metadata writes (a write per second, bursts of 10):
// each may upload an icon, which the per-client rate alone would let one caller repeat.
const metaWritesPerActor, metaWritesBurst = 1, 10

func (h *Handler) setEntryMeta(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	// the caller is authorized before the body is read
	if err := h.o.Releases.MayWriteEntryMeta(r.Context(), a, p["t"], reqType(r), p["source"], p["ext"]); err != nil {
		h.metaErr(w, r, c, p, err)
		return
	}
	if !h.metaLimiter.allow(a.String()) {
		w.Header().Set("Retry-After", h.metaLimiter.retryAfter())
		h.o.Log.Info("api: metadata writes over the actor's rate", "actor", c.logName(), "tenant", p["t"])
		problem(w, http.StatusTooManyRequests, typeRateLimited, "too many metadata writes; try again")
		return
	}
	var in struct {
		Summary *string `json:"summary"`
		Icon    *struct {
			ContentType *string `json:"content_type"`
			Data        *string `json:"data"`
			SHA256      *string `json:"sha256"`
		} `json:"icon"`
	}
	if !body(w, r, &in, maxMetaBody) {
		return
	}
	mw := release.MetaWrite{Summary: in.Summary}
	if ic := in.Icon; ic != nil {
		switch {
		case ic.SHA256 != nil && ic.ContentType == nil && ic.Data == nil:
			mw.Icon = &release.IconInput{Keep: *ic.SHA256}
		case ic.SHA256 == nil && ic.ContentType != nil && ic.Data != nil:
			data, err := base64.StdEncoding.Strict().DecodeString(*ic.Data)
			if err != nil {
				problem(w, http.StatusBadRequest, typeInvalid, "the icon's data is standard base64")
				return
			}
			mw.Icon = &release.IconInput{ContentType: *ic.ContentType, Data: data}
		default:
			problem(w, http.StatusBadRequest, typeInvalid, `the icon is {"content_type", "data"} (a new one), {"sha256"} (the stored one, kept), or null`)
			return
		}
	}
	if c.signed != nil {
		mw.SignedAt, mw.BodySHA256, mw.KeyID, mw.KeyFingerprint = c.signed.Statement.SignedAt, c.signed.Statement.SHA256, keyID(c), c.signed.Fingerprint
	}
	v, err := h.o.Releases.SetEntryMeta(r.Context(), a, p["t"], reqType(r), p["source"], p["ext"], mw)
	if err != nil {
		h.metaErr(w, r, c, p, err)
		return
	}
	h.answerMeta(w, r, c, v)
}

// entryIcon serves an entry's icon: authorization and the entry first, then the object, hashed
// against its row; the type and the headers are kista's own, so the bytes are only ever an image.
func (h *Handler) entryIcon(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	b, err := h.o.Releases.EntryIcon(r.Context(), a, p["t"], reqType(r), p["source"], p["ext"])
	if errors.Is(err, blob.ErrCorrupt) {
		h.o.Log.Error("api: an entry's icon is missing or does not match its row", "tenant", p["t"], "source", p["source"], "ext", p["ext"],
			"error", err)
		notFound(w)
		return
	}
	if err != nil {
		h.metaErr(w, r, c, p, err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", icon.ContentType)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	hd.Set("Content-Disposition", `inline; filename="icon.png"`)
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Vary", "Authorization")
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}
