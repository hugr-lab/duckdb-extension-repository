package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Extension groups (spec 0017): a tenant administrator's; an administrator of a group reads it.

type groupJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Members     int    `json:"members"`
	Grants      *int   `json:"grants,omitempty"` // tenant administrators only
	CreatedAt   string `json:"created_at"`
	CreatedBy   string `json:"created_by"`
	ETag        string `json:"etag"`
}

func groupOf(g store.ExtensionGroup, tenantAdmin bool, c caller) groupJSON {
	o := groupJSON{Name: g.Name, Description: g.Description, Members: g.Members, CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339),
		CreatedBy: createdBy(g.CreatedBy, c), ETag: versionTag(g.Version)}
	if tenantAdmin {
		o.Grants = &g.Grants
	}
	return o
}

func (h *Handler) listGroups(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	prefix, ok := namePrefix(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	gs, tenantAdmin, err := h.o.Auth.ListExtensionGroups(r.Context(), a, p["t"], r.URL.Query().Get("extension"))
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	var kept []store.ExtensionGroup
	for _, g := range gs {
		if strings.HasPrefix(g.Name, prefix) {
			kept = append(kept, g)
		}
	}
	kept, next := paged(kept, func(g store.ExtensionGroup) string { return g.Name }, limit, after)
	out := []groupJSON{}
	for _, g := range kept {
		out = append(out, groupOf(g, tenantAdmin, c))
	}
	reply(w, r, http.StatusOK, list("extension_groups", out, next), "", noStore)
}

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	g, err := h.o.Auth.CreateExtensionGroup(r.Context(), a, p["t"], in.Name, in.Description)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/extension-groups/"+esc(g.Name), groupOf(g, true, c), versionTag(g.Version))
}

func (h *Handler) getGroup(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	g, tenantAdmin, err := h.o.Auth.GetExtensionGroup(r.Context(), a, p["t"], p["g"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, groupOf(g, tenantAdmin, c), versionTag(g.Version), noStore)
}

func (h *Handler) setGroupDescription(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	var in struct {
		Description string `json:"description"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	g, err := h.o.Auth.SetExtensionGroupDescription(r.Context(), a, p["t"], p["g"], in.Description, expected)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, groupOf(g, true, c), versionTag(g.Version), noStore)
}

func (h *Handler) removeGroup(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	if err := h.o.Auth.RemoveExtensionGroup(r.Context(), a, p["t"], p["g"], expected); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}

type memberJSON struct {
	Name    string `json:"name"`
	AddedAt string `json:"added_at"`
	AddedBy string `json:"added_by"`
}

func (h *Handler) groupMembers(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	prefix, ok := namePrefix(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	ms, err := h.o.Auth.ExtensionGroupMembers(r.Context(), a, p["t"], p["g"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	var kept []store.GroupMember
	for _, m := range ms {
		if strings.HasPrefix(m.Name, prefix) {
			kept = append(kept, m)
		}
	}
	kept, next := paged(kept, func(m store.GroupMember) string { return m.Name }, limit, after)
	out := []memberJSON{}
	for _, m := range kept {
		out = append(out, memberJSON{Name: m.Name, AddedAt: m.AddedAt.UTC().Format(time.RFC3339), AddedBy: createdBy(m.AddedBy, c)})
	}
	reply(w, r, http.StatusOK, list("members", out, next), "", noStore)
}

func (h *Handler) changeGroupMembers(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	added, removed, err := h.o.Auth.ChangeExtensionGroupMembers(r.Context(), a, p["t"], p["g"], in.Add, in.Remove)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"added": nonNil(added), "removed": nonNil(removed)}, "", noStore)
}
