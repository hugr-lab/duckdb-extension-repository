package api

import (
	"net/http"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
)

// Spec 0031: a name's source in a channel.

type sourceJSON struct {
	Name      string         `json:"name"`
	Source    string         `json:"source"` // "": none; the next release of the name sets it
	SetBy     string         `json:"set_by,omitempty"`
	SetAt     string         `json:"set_at,omitempty"`
	Available map[string]int `json:"available"` // the sources with releases of the name here, each with its count
	// spec 0033: the rule's mode, the newest version the channel holds and the newest in the
	// catalogue (of the source), the placement pass's last outcome
	Mode      string `json:"mode,omitempty"`
	Current   string `json:"current,omitempty"`
	Newest    string `json:"newest,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	OutcomeAt string `json:"outcome_at,omitempty"`
}

func sourceOf(v release.SourceView, c caller) sourceJSON {
	o := sourceJSON{Name: v.Name, Source: v.Source, Available: v.Available}
	if v.Source != "" {
		o.SetBy, o.SetAt = createdBy(v.SetBy, c), timeOf(v.SetAt)
		o.Mode, o.Current, o.Newest, o.Outcome = v.Mode, v.Current, v.Newest, v.Outcome
		if v.Outcome != "" {
			o.OutcomeAt = timeOf(v.OutcomeAt)
		}
	}
	return o
}

func (h *Handler) channelSources(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	vs, err := h.o.Releases.Sources(r.Context(), a, p["t"], p["c"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []sourceJSON{}
	for _, v := range vs {
		out = append(out, sourceOf(v, c))
	}
	reply(w, r, http.StatusOK, map[string]any{"sources": out}, "", noStore)
}

func (h *Handler) channelSource(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	v, err := h.o.Releases.Source(r.Context(), a, p["t"], p["c"], p["name"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, sourceOf(v, c), "", noStore)
}

func (h *Handler) setChannelSource(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Source string `json:"source"`
		Mode   string `json:"mode"` // spec 0033: auto or manual; absent keeps the rule's (manual for a new one)
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	v, err := h.o.Releases.SetSource(r.Context(), a, p["t"], p["c"], p["name"], in.Source, in.Mode)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, sourceOf(v, c), "", noStore)
}

func (h *Handler) clearChannelSource(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Releases.ClearSource(r.Context(), a, p["t"], p["c"], p["name"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}
