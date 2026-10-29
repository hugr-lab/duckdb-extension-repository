package api

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
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
	case errors.Is(err, licenses.ErrSources):
		problem(w, http.StatusConflict, typeConflict, strings.TrimPrefix(err.Error(), "licenses: ")+"; name one with ?source=")
	case errors.Is(err, licenses.ErrDistributorsEntry):
		// spec 0037: a refusal, recorded with what it refused (the entry as named and resolved, the
		// name marked when it was read from an unverified token); its 404 says why
		var de *licenses.DistributorsEntry
		f := map[string]any{}
		detail := "a distributor's entry: its licence manager or a server administrator writes it"
		if errors.As(err, &de) {
			f = map[string]any{"name": de.Name, "source": de.Source, "entry": de.Entry, "operation": de.Op}
			if de.NameUnverified {
				f["name_unverified"] = true
			}
		}
		note(w, func(sw *statusWriter) { sw.refused, sw.refusal = true, f })
		writeProblem(w, http.StatusNotFound, typeDistributorsEntry, detail, true)
	default:
		h.serviceErr(w, err, false)
	}
}

// olderRefused records an import refused by the rule older (spec 0037) as a license.refused event:
// the licence (verified before the rule was checked), its issuer and the entry named. Nothing for any
// other error.
func (h *Handler) olderRefused(r *http.Request, c caller, source, token string, err error) {
	var le *license.Error
	if h.o.Events == nil || !errors.Is(err, licenses.ErrRefused) || !errors.As(err, &le) || le.Rule != licenses.RuleOlder {
		return
	}
	f := map[string]any{"rule": le.Rule, "source": source}
	name := ""
	if t, perr := license.Parse(token); perr == nil {
		name = t.Extension
		f["name"], f["license"], f["issuer"] = t.Extension, t.ID, t.Issuer
	}
	h.o.Events.Refusal(r.Context(), c.tenant.ID, c.eventActor(), "license.refused", audit.Refused, "license:"+name, f)
}

// querySource reads a licence route's ?source= (spec 0035: the licence's entry): "" when the query
// names none; one named twice is refused (400), as a repeated ?type= is.
func querySource(w http.ResponseWriter, r *http.Request) (string, bool) {
	vs := r.URL.Query()["source"]
	switch len(vs) {
	case 0:
		return "", true
	case 1:
		return vs[0], true
	}
	problem(w, http.StatusBadRequest, typeInvalid, "?source= names one entry: it is given once")
	return "", false
}

// licenseKeys answers the licence keys of the tenant's live builds, by name (spec 0021), each with
// the entries for which it is live, of one entry with ?source= (spec 0035).
func (h *Handler) licenseKeys(w http.ResponseWriter, r *http.Request, c caller, p params) {
	source, ok := querySource(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	ks, err := h.o.Licenses.LicenseKeys(r.Context(), a, p["t"], reqType(r), p["name"], source)
	if err != nil {
		h.licenseErr(w, err)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"names": ks}, "", noStore)
}

func (h *Handler) listLicenses(w http.ResponseWriter, r *http.Request, c caller, p params) {
	source, ok := querySource(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	vs, err := h.o.Licenses.List(r.Context(), a, p["t"], reqType(r), source)
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
	source, ok := querySource(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	v, err := h.o.Licenses.Get(r.Context(), a, p["t"], reqType(r), p["name"], source)
	if err != nil {
		h.licenseErr(w, err)
		return
	}
	reply(w, r, http.StatusOK, licenseOf(v), versionTag(v.Version), noStore)
}

// importLicense stores a licence posted as the body (the compact JWS), in the entry ?source= names
// (spec 0035: required). Without If-Match only an entry with no licence of the name takes it;
// If-Match replaces the licence stored there.
func (h *Handler) importLicense(w http.ResponseWriter, r *http.Request, c caller, p params) {
	source, ok := querySource(w, r)
	if !ok {
		return
	}
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
	token := strings.TrimSpace(string(b))
	v, err := h.o.Licenses.Import(r.Context(), a, p["t"], reqType(r), source, token, expected)
	if err != nil {
		h.olderRefused(r, c, source, token, err)
		h.licenseErr(w, err)
		return
	}
	if v.Created {
		created(w, r, "/api/v1/tenants/"+p["t"]+"/licenses/"+v.Name+sourceQuery(r, v.Entry), licenseOf(v), versionTag(v.Version))
		return
	}
	reply(w, r, http.StatusOK, licenseOf(v), versionTag(v.Version), noStore)
}

func (h *Handler) removeLicense(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	source, ok := querySource(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	if err := h.o.Licenses.Remove(r.Context(), a, p["t"], reqType(r), p["name"], source, expected); err != nil {
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
		source, ok := querySource(w, r)
		if !ok {
			return
		}
		a, _ := c.actor()
		v, err := h.o.Licenses.SetVisibility(r.Context(), a, p["t"], reqType(r), p["name"], source, visibility, expected)
		if err != nil {
			h.licenseErr(w, err)
			return
		}
		reply(w, r, http.StatusOK, licenseOf(v), versionTag(v.Version), noStore)
	}
}

// sourceQuery is a licence's location's query: its entry (spec 0035), and its type when it is not
// the default (spec 0034).
func sourceQuery(r *http.Request, source string) string {
	q := url.Values{"source": {source}}
	if t := reqType(r); t != pkgtype.Default {
		q.Set("type", t)
	}
	return "?" + q.Encode()
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
