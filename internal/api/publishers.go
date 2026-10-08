package api

import (
	"net/http"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0008: publishers and their credentials (tenant administrators).

type githubJSON struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	OwnerID      string `json:"owner_id"`
	RepositoryID string `json:"repository_id"`
	Workflow     string `json:"workflow"`
	Ref          string `json:"ref,omitempty"`
	Environment  string `json:"environment,omitempty"`
	CreatedAt    string `json:"created_at"`
	CreatedBy    string `json:"created_by"`
}

type publisherJSON struct {
	Name      string       `json:"name"`
	CreatedAt string       `json:"created_at"`
	CreatedBy string       `json:"created_by"`
	GitHub    []githubJSON `json:"github"`
}

func githubOf(g store.GitHubCredential, c caller) githubJSON {
	return githubJSON{ID: g.ID, Provider: g.Provider, OwnerID: g.OwnerID, RepositoryID: g.RepositoryID, Workflow: g.Workflow,
		Ref: g.Ref, Environment: g.Environment, CreatedAt: timeOf(g.CreatedAt), CreatedBy: createdBy(g.CreatedBy, c)}
}

func publisherOf(p store.Publisher, c caller) publisherJSON {
	o := publisherJSON{Name: p.Name, CreatedAt: timeOf(p.CreatedAt), CreatedBy: createdBy(p.CreatedBy, c), GitHub: []githubJSON{}}
	for _, g := range p.GitHub {
		o.GitHub = append(o.GitHub, githubOf(g, c))
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
	out := []publisherJSON{}
	for _, x := range ps {
		out = append(out, publisherOf(x, c))
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
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/publishers/"+esc(pb.Name), publisherOf(pb, c), "-")
}

func (h *Handler) getPublisher(w http.ResponseWriter, r *http.Request, c caller, p params) {
	ps, ok := h.tenantPublishers(w, r, c, p)
	if !ok {
		return
	}
	for _, x := range ps {
		if x.Name == p["name"] {
			reply(w, r, http.StatusOK, publisherOf(x, c), "", noStore)
			return
		}
	}
	notFound(w)
}

func (h *Handler) removePublisher(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Auth.RemovePublisher(r.Context(), a, p["t"], p["name"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}

func (h *Handler) listGitHub(w http.ResponseWriter, r *http.Request, c caller, p params) {
	ps, ok := h.tenantPublishers(w, r, c, p)
	if !ok {
		return
	}
	for _, x := range ps {
		if x.Name == p["name"] {
			reply(w, r, http.StatusOK, map[string]any{"github": publisherOf(x, c).GitHub}, "", noStore)
			return
		}
	}
	notFound(w)
}

func (h *Handler) addGitHub(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Provider     string `json:"provider"`
		OwnerID      string `json:"owner_id"`
		RepositoryID string `json:"repository_id"`
		Workflow     string `json:"workflow"`
		Ref          string `json:"ref"`
		Environment  string `json:"environment"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	g, err := h.o.Auth.AddGitHubCredential(r.Context(), a, p["t"], p["name"], store.GitHubCredential{Provider: in.Provider,
		OwnerID: in.OwnerID, RepositoryID: in.RepositoryID, Workflow: in.Workflow, Ref: in.Ref, Environment: in.Environment})
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/publishers/"+esc(p["name"])+"/github/"+esc(g.ID), githubOf(g, c), "-")
}

func (h *Handler) getGitHub(w http.ResponseWriter, r *http.Request, c caller, p params) {
	ps, ok := h.tenantPublishers(w, r, c, p)
	if !ok {
		return
	}
	for _, x := range ps {
		if x.Name != p["name"] {
			continue
		}
		for _, g := range x.GitHub {
			if g.ID == p["id"] {
				reply(w, r, http.StatusOK, githubOf(g, c), "", noStore)
				return
			}
		}
	}
	notFound(w)
}

func (h *Handler) removeGitHub(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Auth.RemoveGitHubCredential(r.Context(), a, p["t"], p["name"], p["id"]); err != nil {
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
		pubs = append(pubs, map[string]string{"publisher": m.Publisher.Name, "credential": "github:" + m.Credential})
		ids[m.Publisher.ID] = true
	}
	grants := []grantOut{}
	for _, g := range c.ta.Grants {
		if g.Kind == store.PrincipalPublisher && ids[g.PublisherID] {
			grants = append(grants, grantOut{Principal: g.Principal(), Verbs: g.Verbs, Channel: g.ChannelName, Extension: g.Extension})
		}
	}
	return map[string]any{"tenant": c.tenant.Name, "provider": c.pub.Provider, "publishers": pubs, "grants": grants,
		"holds_admin": false}
}
