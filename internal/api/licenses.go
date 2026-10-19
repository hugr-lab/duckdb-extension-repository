package api

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
)

// Licences (spec 0012 phase 1a): a tenant's licences, imported by its administrators or relayed
// from its credentialed upstreams; kista issues none.

type licenseJSON struct {
	licenses.View
	ETag string `json:"etag"`
}

func licenseOf(v licenses.View) licenseJSON { return licenseJSON{View: v, ETag: versionTag(v.Version)} }

func licenseRoute(f handler) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		if h.o.Licenses == nil {
			notFound(w)
			return
		}
		f(h, w, r, c, p)
	}
}

// licenseErr maps the licence service's errors; the rest are serviceErr's.
func (h *Handler) licenseErr(w http.ResponseWriter, err error) {
	var le *license.Error
	switch {
	case errors.Is(err, licenses.ErrOff):
		problem(w, http.StatusConflict, typeConflict, "licensing is off on this server (licenses.enabled: false)")
	case errors.Is(err, licenses.ErrRefused) && errors.As(err, &le):
		problem(w, http.StatusBadRequest, typeInvalid, "the licence is refused ("+le.Rule+"): "+strings.TrimPrefix(le.Error(), "license: "))
	case errors.Is(err, licenses.ErrNotLive):
		problem(w, http.StatusBadRequest, typeInvalid, "the licence is not in force: "+strings.TrimPrefix(err.Error(), licenses.ErrNotLive.Error()+": "))
	case errors.Is(err, licenses.ErrExists):
		problem(w, http.StatusConflict, typeConflict, "a licence is stored for this extension: replace it with If-Match")
	case errors.Is(err, licenses.ErrUpstream):
		problem(w, http.StatusConflict, typeConflict, "the upstream relays no licences: it needs a credential, and not bundle mode")
	case errors.Is(err, licenses.ErrPaused):
		problem(w, http.StatusConflict, typeConflict, "the upstream is paused: resume it to relay its licences")
	default:
		h.serviceErr(w, err, false)
	}
}

// licenseKeys answers the licence keys of the tenant's live builds, by name (spec 0021).
func (h *Handler) licenseKeys(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	ks, err := h.o.Licenses.LicenseKeys(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.licenseErr(w, err)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"names": ks}, "", noStore)
}

func (h *Handler) listLicenses(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	vs, err := h.o.Licenses.List(r.Context(), a, p["t"])
	if err != nil {
		h.licenseErr(w, err)
		return
	}
	out := make([]licenseJSON, 0, len(vs))
	for _, v := range vs {
		out = append(out, licenseOf(v))
	}
	reply(w, r, http.StatusOK, map[string]any{"licenses": out}, "", noStore)
}

func (h *Handler) getLicense(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	v, err := h.o.Licenses.Get(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.licenseErr(w, err)
		return
	}
	reply(w, r, http.StatusOK, licenseOf(v), versionTag(v.Version), noStore)
}

// importLicense stores a licence posted as the body (the compact JWS). Without If-Match only a name
// with no licence takes it; If-Match replaces the licence stored.
func (h *Handler) importLicense(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected := int64(licenses.Create)
	if r.Header.Get("If-Match") != "" {
		n, ok := expectedVersion(w, r)
		if !ok {
			return
		}
		expected = n
	}
	if r.ContentLength <= 0 || r.ContentLength > license.MaxSize {
		problem(w, http.StatusBadRequest, typeInvalid, "the body is the licence: 1 to 16384 bytes, with a Content-Length")
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength))
	if err != nil || int64(len(b)) != r.ContentLength {
		problem(w, http.StatusBadRequest, typeInvalid, "the body ended before its Content-Length")
		return
	}
	a, _ := c.actor()
	v, err := h.o.Licenses.Import(r.Context(), a, p["t"], strings.TrimSpace(string(b)), expected)
	if err != nil {
		h.licenseErr(w, err)
		return
	}
	if v.Created {
		created(w, r, "/api/v1/tenants/"+p["t"]+"/licenses/"+v.Name, licenseOf(v), versionTag(v.Version))
		return
	}
	reply(w, r, http.StatusOK, licenseOf(v), versionTag(v.Version), noStore)
}

func (h *Handler) removeLicense(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	if err := h.o.Licenses.Remove(r.Context(), a, p["t"], p["name"], expected); err != nil {
		h.licenseErr(w, err)
		return
	}
	noContent(w)
}

// licenseVisibility is public or private (If-Match).
func licenseVisibility(visibility string) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		expected, ok := expectedVersion(w, r)
		if !ok {
			return
		}
		a, _ := c.actor()
		v, err := h.o.Licenses.SetVisibility(r.Context(), a, p["t"], p["name"], visibility, expected)
		if err != nil {
			h.licenseErr(w, err)
			return
		}
		reply(w, r, http.StatusOK, licenseOf(v), versionTag(v.Version), noStore)
	}
}

// syncLicenses asks for an upstream's licence relay now (202), and answers its last run.
func (h *Handler) syncLicenses(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Licenses.Sync(r.Context(), a, p["t"], p["name"]); err != nil {
		h.licenseErr(w, err)
		return
	}
	h.lastLicenseSync(w, r, c, p, http.StatusAccepted)
}

// licenseSync answers an upstream's last licence relay run.
func (h *Handler) licenseSync(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.lastLicenseSync(w, r, c, p, http.StatusOK)
}

func (h *Handler) lastLicenseSync(w http.ResponseWriter, r *http.Request, c caller, p params, status int) {
	a, _ := c.actor()
	at, res, err := h.o.Licenses.LastSync(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.licenseErr(w, err)
		return
	}
	reply(w, r, status, map[string]any{"last_run_at": timeOf(at), "result": res}, "", noStore)
}
