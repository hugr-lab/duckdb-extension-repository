package api

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0008: publishers and their credentials (tenant administrators).

type publisherKeyJSON struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	Title       string `json:"title"`
	PublicKey   string `json:"public_key"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	CreatedAt   string `json:"created_at"`
	CreatedBy   string `json:"created_by"`
	LastUsedAt  string `json:"last_used_at,omitempty"`
}

type publisherJSON struct {
	Name      string             `json:"name"`
	CreatedAt string             `json:"created_at"`
	CreatedBy string             `json:"created_by"`
	Keys      []publisherKeyJSON `json:"keys"`
	// LicenseKeys: a trusted source of licence keys (spec 0021)
	LicenseKeys bool `json:"license_keys"`
}

func publisherKeyOf(k store.PublisherKey, c caller) publisherKeyJSON {
	return publisherKeyJSON{ID: k.ID, Type: k.Type, Fingerprint: k.Fingerprint, Title: k.Title, PublicKey: k.PublicKey,
		ExpiresAt: timeOf(k.ExpiresAt), CreatedAt: timeOf(k.CreatedAt), CreatedBy: createdBy(k.CreatedBy, c), LastUsedAt: timeOf(k.LastUsedAt)}
}

func publisherOf(p store.Publisher, keys []store.PublisherKey, c caller) publisherJSON {
	o := publisherJSON{Name: p.Name, CreatedAt: timeOf(p.CreatedAt), CreatedBy: createdBy(p.CreatedBy, c), Keys: []publisherKeyJSON{},
		LicenseKeys: p.LicenseKeys}
	for _, k := range keys {
		o.Keys = append(o.Keys, publisherKeyOf(k, c))
	}
	return o
}

func (h *Handler) tenantPublishers(w http.ResponseWriter, r *http.Request, c caller, p params) ([]store.Publisher, bool) {
	a, _ := c.actor()
	ps, err := h.o.Auth.ListPublishers(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return nil, false
	}
	return ps, true
}

func (h *Handler) listPublishers(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	ps, ok := h.tenantPublishers(w, r, c, p)
	if !ok {
		return
	}
	ps, next := paged(ps, func(x store.Publisher) string { return x.Name }, limit, after)
	a, _ := c.actor()
	keys, err := h.o.Auth.TenantPublisherKeys(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []publisherJSON{}
	for _, x := range ps {
		out = append(out, publisherOf(x, keys[x.ID], c))
	}
	reply(w, r, http.StatusOK, list("publishers", out, next), "", noStore)
}

func (h *Handler) addPublisher(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Name string `json:"name"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	pb, err := h.o.Auth.AddPublisher(r.Context(), a, p["t"], in.Name)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/publishers/"+esc(pb.Name), publisherOf(pb, nil, c), "-")
}

func (h *Handler) getPublisher(w http.ResponseWriter, r *http.Request, c caller, p params) {
	ps, ok := h.tenantPublishers(w, r, c, p)
	if !ok {
		return
	}
	for _, x := range ps {
		if x.Name == p["name"] {
			a, _ := c.actor()
			keys, err := h.o.Auth.ListPublisherKeys(r.Context(), a, p["t"], x.Name)
			if errors.Is(err, authz.ErrDenied) && c.distribution {
				keys, err = nil, nil // an unmarked publisher's keys are not a distribution admin's to read
			}
			if err != nil {
				h.serviceErr(w, err, false)
				return
			}
			reply(w, r, http.StatusOK, publisherOf(x, keys, c), "", noStore)
			return
		}
	}
	notFound(w)
}

// publisherLicenseKeys marks a publisher a source of licence keys, or unmarks it (spec 0021).
func publisherLicenseKeys(on bool) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		a, _ := c.actor()
		pb, err := h.o.Auth.SetPublisherLicenseKeys(r.Context(), a, p["t"], p["name"], on)
		if err != nil {
			h.serviceErr(w, err, false)
			return
		}
		keys, err := h.o.Auth.ListPublisherKeys(r.Context(), a, p["t"], pb.Name)
		if errors.Is(err, authz.ErrDenied) && c.distribution {
			keys, err = nil, nil // unmarked: its keys are the tenant administrators' to read
		}
		if err != nil {
			h.serviceErr(w, err, false)
			return
		}
		reply(w, r, http.StatusOK, publisherOf(pb, keys, c), "", noStore)
	}
}

func (h *Handler) removePublisher(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Auth.RemovePublisher(r.Context(), a, p["t"], p["name"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}

// publisherWhoami is what a publisher's credential is: its publishers, the matched credentials, and
// their grants.
func publisherWhoami(c caller) map[string]any {
	var pubs []map[string]string
	ids := map[string]bool{}
	for _, m := range c.pub.Publishers {
		pubs = append(pubs, map[string]string{"publisher": m.Publisher.Name, "credential": m.Credential})
		ids[m.Publisher.ID] = true
	}
	grants := []grantOut{}
	for _, g := range slices.Concat(c.ta.Grants, c.ta.GroupGrants) {
		if g.Kind == store.PrincipalPublisher && ids[g.PublisherID] {
			grants = append(grants, grantOutOf(g))
		}
	}
	return map[string]any{"tenant": c.tenant.Name, "provider": c.pub.Provider, "publishers": pubs, "grants": grants,
		"holds_admin": false}
}

// listPublisherKeys lists a publisher's keys.
func (h *Handler) listPublisherKeys(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	ks, err := h.o.Auth.ListPublisherKeys(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []publisherKeyJSON{}
	for _, k := range ks {
		out = append(out, publisherKeyOf(k, c))
	}
	reply(w, r, http.StatusOK, map[string]any{"keys": out}, "", noStore)
}

// getPublisherKey reads one of a publisher's keys.
func (h *Handler) getPublisherKey(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	ks, err := h.o.Auth.ListPublisherKeys(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	for _, k := range ks {
		if k.ID == p["id"] {
			reply(w, r, http.StatusOK, publisherKeyOf(k, c), "", noStore)
			return
		}
	}
	notFound(w)
}

// addPublisherKey registers a publisher's public SSH key (spec 0023).
func (h *Handler) addPublisherKey(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		PublicKey string `json:"public_key"` // an OpenSSH line
		ExpiresAt string `json:"expires_at"` // RFC 3339, within a year; "": never
	}
	if !body(w, r, &in) {
		return
	}
	var exp time.Time
	if in.ExpiresAt != "" {
		var err error
		if exp, err = time.Parse(time.RFC3339, in.ExpiresAt); err != nil {
			problem(w, http.StatusBadRequest, typeInvalid, "expires_at is an RFC 3339 time within a year, or absent")
			return
		}
	}
	a, _ := c.actor()
	k, err := h.o.Auth.AddPublisherKey(r.Context(), a, p["t"], p["name"], in.PublicKey, exp)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/publishers/"+esc(p["name"])+"/keys/"+esc(k.ID), publisherKeyOf(k, c), "-")
}

// removePublisherKey removes a publisher's key; it stops working at once.
func (h *Handler) removePublisherKey(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Auth.RemovePublisherKey(r.Context(), a, p["t"], p["name"], p["id"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}
