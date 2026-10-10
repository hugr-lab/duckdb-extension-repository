package api

import (
	"net/http"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Console is what the administration console's public configuration routes say (spec 0015).
type Console struct {
	Environment      string
	AdminTokenMaxAge int64  // seconds
	Audience         string // the server tokens' audience the console asks for
	Issuers          []tenants.ConsoleIssuer
}

func (h *Handler) serverConsole(w http.ResponseWriter, r *http.Request, _ caller, _ params) {
	c := h.o.Console
	if c == nil {
		notFound(w)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"environment": c.Environment, "admin_token_max_age": c.AdminTokenMaxAge,
		"audience": c.Audience, "issuers": nonNil(c.Issuers)}, "", noStore)
}

func (h *Handler) tenantConsole(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	if h.o.Console == nil || h.o.Auth == nil {
		notFound(w)
		return
	}
	iss, err := h.o.Auth.ConsoleIssuers(r.Context(), c.tenant)
	if err != nil {
		h.fail(w, err)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"issuers": nonNil(iss)}, "", noStore)
}

type consoleClientJSON struct {
	ClientID          string   `json:"client_id"`
	Scopes            []string `json:"scopes"`
	AudienceParameter string   `json:"audience_parameter"`
	Audience          string   `json:"audience"`
}

// issuerIfMatch reads the If-Match naming an issuer record: the record's id ("" for *).
func issuerIfMatch(w http.ResponseWriter, r *http.Request) (string, bool) {
	tag, ok := ifMatch(w, r)
	if !ok {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(tag, `"`), `"`)
	if tag != "" && (id == "" || `"`+id+`"` != tag) {
		problem(w, http.StatusPreconditionFailed, typePrecondition, "it changed: read it again")
		return "", false
	}
	return id, true
}

// exactIssuer reads the If-Match of an issuer record's console client: the record's id, never *,
// so a record re-added under the name never takes a client meant for the old one (spec 0015).
func exactIssuer(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := issuerIfMatch(w, r)
	if ok && id == "" {
		problem(w, http.StatusPreconditionRequired, typePrecondition, `If-Match is the issuer record's ETag, not *`)
		return "", false
	}
	return id, ok
}

func (h *Handler) setConsoleClient(w http.ResponseWriter, r *http.Request, c caller, p params) {
	id, ok := exactIssuer(w, r)
	if !ok {
		return
	}
	var in consoleClientJSON
	if !body(w, r, &in) {
		return
	}
	if len(in.Scopes) == 0 {
		in.Scopes = []string{"openid", "profile", "offline_access"}
	}
	a, _ := c.actor()
	cc, err := h.o.Auth.SetConsoleClient(r.Context(), a, p["t"], p["name"], id, store.ConsoleClient{ClientID: in.ClientID,
		Scopes: in.Scopes, AudienceParameter: in.AudienceParameter, Audience: in.Audience})
	if err != nil {
		h.serviceErr(w, err, true)
		return
	}
	reply(w, r, http.StatusOK, consoleClientJSON{ClientID: cc.ClientID, Scopes: cc.Scopes, AudienceParameter: cc.AudienceParameter,
		Audience: cc.Audience}, "", noStore)
}

func (h *Handler) removeConsoleClient(w http.ResponseWriter, r *http.Request, c caller, p params) {
	id, ok := exactIssuer(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	if err := h.o.Auth.RemoveConsoleClient(r.Context(), a, p["t"], p["name"], id); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}
