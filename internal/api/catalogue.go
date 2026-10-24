package api

import (
	"errors"
	"net/http"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033: a tenant's catalogue, and placing its builds into a channel by hand.

type catalogueEntryJSON struct {
	Source   string            `json:"source"`
	Name     string            `json:"name"`
	Versions []string          `json:"versions"` // newest first
	Rules    map[string]string `json:"rules"`    // the channels whose rule takes the source, with its mode
}

func (h *Handler) catalogue(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	q := r.URL.Query()
	es, err := h.o.Releases.Catalogue(r.Context(), a, p["t"], q.Get("source"), q.Get("name"))
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []catalogueEntryJSON{}
	for _, e := range es {
		out = append(out, catalogueEntryJSON{Source: e.Source, Name: e.Name, Versions: e.Versions, Rules: e.Rules})
	}
	reply(w, r, http.StatusOK, map[string]any{"entries": out}, "", noStore)
}

type catalogueBuildJSON struct {
	ID            string         `json:"id"`
	Platform      string         `json:"platform"`
	Slot          string         `json:"slot"`
	ABI           string         `json:"abi"`
	DuckDBVersion string         `json:"duckdb_version,omitempty"`
	CAPI          string         `json:"c_api,omitempty"`
	BodyHash      string         `json:"body_hash"`
	Visibility    string         `json:"visibility"`
	Origin        string         `json:"origin"`
	Checked       bool           `json:"checked"`
	CreatedAt     string         `json:"created_at"`
	Placed        []store.Placed `json:"placed"`
}

func (h *Handler) catalogueEntry(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	vs, err := h.o.Releases.CatalogueVersions(r.Context(), a, p["t"], p["source"], p["ext"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	type versionJSON struct {
		Version string               `json:"version"`
		Builds  []catalogueBuildJSON `json:"builds"`
	}
	out := []versionJSON{}
	for _, v := range vs {
		vj := versionJSON{Version: v.Version, Builds: []catalogueBuildJSON{}}
		for _, b := range v.Builds {
			bj := catalogueBuildJSON{ID: b.ID, Platform: b.Platform, Slot: b.Slot(), ABI: b.ABI, DuckDBVersion: b.DuckDBVersion,
				BodyHash: b.BodyHash, Visibility: b.Visibility, Origin: b.Origin, Checked: b.Checked, CreatedAt: timeOf(b.CreatedAt),
				Placed: b.Placed}
			if b.CAPI != nil {
				bj.CAPI = b.CAPI.String()
			}
			if bj.Placed == nil {
				bj.Placed = []store.Placed{}
			}
			vj.Builds = append(vj.Builds, bj)
		}
		out = append(out, vj)
	}
	reply(w, r, http.StatusOK, map[string]any{"source": p["source"], "name": p["ext"], "versions": out}, "", noStore)
}

func (h *Handler) placeRelease(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Source     string `json:"source"`
		Version    string `json:"version"`
		Visibility string `json:"visibility"`
		Current    *bool  `json:"current"`
	}
	if !body(w, r, &in) {
		return
	}
	o := release.PlaceOptions{Source: in.Source, Version: in.Version}
	switch in.Visibility {
	case "":
	case store.Private:
		o.Private = true
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "visibility can only be narrowed to private; a placement keeps the build's")
		return
	}
	o.NotCurrent = in.Current != nil && !*in.Current
	a, _ := c.actor()
	rels, existed, err := h.o.Releases.Place(r.Context(), a, p["t"], p["c"], p["ext"], o)
	if errors.Is(err, release.ErrNoRule) {
		problem(w, http.StatusConflict, typeSource, publicRefusal(err))
		return
	}
	if err != nil {
		h.publishErr(w, err, false)
		return
	}
	h.answerReleases(w, r, c, p, rels, existed, false)
}
