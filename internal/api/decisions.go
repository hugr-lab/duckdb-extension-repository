package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
)

// Spec 0041: the administrators' decisions on the decision queue (Skip and Dismiss).

type decisionJSON struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Channel     string          `json:"channel,omitempty"` // a channel's decision; omitted for the tenant's
	Source      string          `json:"source"`
	Name        string          `json:"name"`
	Version     string          `json:"version,omitempty"`   // a skip's
	Situation   json.RawMessage `json:"situation,omitempty"` // a dismissal's
	DecidedBy   string          `json:"decided_by"`
	DecidedAt   string          `json:"decided_at"`
	PackageType string          `json:"package_type,omitempty"` // omitted when it is the default
	// ETag is the decision's row version as a strong ETag, in lists too: any client can Undo
	ETag string `json:"etag"`
}

func decisionOf(d release.DecisionView, c caller) decisionJSON {
	o := decisionJSON{ID: d.ID, Kind: d.Kind, Channel: d.Channel, Source: d.SourceLabel, Name: d.Name, Version: d.PackageVersion,
		DecidedBy: createdBy(d.DecidedBy, c), DecidedAt: timeOf(d.DecidedAt), PackageType: shownType(d.PackageType), ETag: versionTag(d.Version)}
	if d.Situation != "" {
		o.Situation = json.RawMessage(d.Situation)
	}
	return o
}

func (h *Handler) answerDecisions(w http.ResponseWriter, r *http.Request, c caller, ds []release.DecisionView, limit int) {
	out := []decisionJSON{}
	for _, d := range ds {
		out = append(out, decisionOf(d, c))
	}
	next := ""
	if len(out) > limit { // a full page: the store read one more
		out = out[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(out[len(out)-1].ID))
	}
	reply(w, r, http.StatusOK, list("decisions", out, next), "", noStore)
}

// tenantDecisions lists the tenant's decisions: GET …/decisions[?type=&channel=&cursor=&limit=].
func (h *Handler) tenantDecisions(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	typ := ""
	if t, named, _ := queryType(r); named {
		typ = t
	}
	a, _ := c.actor()
	ds, err := h.o.Releases.Decisions(r.Context(), a, p["t"], typ, r.URL.Query().Get("channel"), after, limit+1)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	h.answerDecisions(w, r, c, ds, limit)
}

// channelDecisions lists a channel's decisions: GET …/channels/{c}/decisions[?cursor=&limit=].
func (h *Handler) channelDecisions(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	ds, err := h.o.Releases.ChannelDecisions(r.Context(), a, p["t"], p["c"], after, limit+1)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	h.answerDecisions(w, r, c, ds, limit)
}

// setDecision writes or replaces a decision: PUT …/decisions {"kind", "channel"?, "source", "name",
// "version"?, "situation"?, "type"?}; 201 when new, 200 when it replaced one.
func (h *Handler) setDecision(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Kind      string          `json:"kind"`
		Channel   string          `json:"channel"`
		Source    string          `json:"source"`
		Name      string          `json:"name"`
		Version   string          `json:"version"`
		Situation json.RawMessage `json:"situation"`
		Type      string          `json:"type"`
	}
	if !body(w, r, &in, maxBody) {
		return
	}
	if in.Type != "" && !pkgtype.Known(in.Type) {
		problem(w, http.StatusBadRequest, typeInvalid, "type is a package type this server knows")
		return
	}
	if string(in.Situation) == "null" {
		in.Situation = nil
	}
	a, _ := c.actor()
	d, created, err := h.o.Releases.SetDecision(r.Context(), a, p["t"], release.DecisionInput{Kind: in.Kind, Channel: in.Channel,
		Source: in.Source, Name: in.Name, Version: in.Version, Situation: in.Situation, PackageType: in.Type})
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	reply(w, r, code, decisionOf(d, c), versionTag(d.Version), noStore)
}

// clearDecision removes a decision (Undo): DELETE …/decisions/{id} with If-Match, its etag (412
// when another administrator changed it since).
func (h *Handler) clearDecision(w http.ResponseWriter, r *http.Request, c caller, p params) {
	v, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	if err := h.o.Releases.ClearDecision(r.Context(), a, p["t"], p["id"], v); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}
