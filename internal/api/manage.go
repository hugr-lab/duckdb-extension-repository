package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// maxBody is the largest management request body.
const maxBody = 64 << 10

const noStore = "no-store"

// body reads a JSON request body into v (after the decision): application/json exactly, at most
// 64 KiB, within a read deadline, no unknown or duplicate keys, nothing after the object.
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, ps, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" || len(ps) > 1 || len(ps) == 1 && !strings.EqualFold(ps["charset"], "utf-8") {
		problem(w, http.StatusUnsupportedMediaType, typeMediaType, "the body is application/json")
		return false
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Second))
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem(w, http.StatusRequestEntityTooLarge, typeTooLarge, "the body is at most 64 KiB")
		} else {
			problem(w, http.StatusBadRequest, typeInvalid, "the body could not be read")
		}
		return false
	}
	if t := bytes.TrimSpace(b); !json.Valid(b) || len(t) == 0 || t[0] != '{' {
		problem(w, http.StatusBadRequest, typeInvalid, "the body is not one JSON object")
		return false
	}
	if err := auth.NoDuplicateKeys(b); err != nil {
		problem(w, http.StatusBadRequest, typeInvalid, "the body has a duplicate key")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		problem(w, http.StatusBadRequest, typeInvalid, "the body does not match the request: "+jsonProblem(err))
		return false
	}
	return true
}

// jsonProblem describes a decoding error without echoing values.
func jsonProblem(err error) string {
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &te):
		return "a value of the wrong type for " + te.Field
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return "an unknown field"
	}
	return "not valid JSON"
}

// serviceErr answers a service error. bodyRef says a not-found refers to a name in the body (400),
// not to the path (404).
func (h *Handler) serviceErr(w http.ResponseWriter, err error, bodyRef bool) {
	switch {
	case errors.Is(err, authz.ErrDenied):
		note(w, func(sw *statusWriter) { sw.refused = true })
		notFound(w)
	case errors.Is(err, store.ErrNotFound):
		if bodyRef {
			problem(w, http.StatusBadRequest, typeInvalid, "a name in the body does not exist")
		} else {
			notFound(w)
		}
	case errors.Is(err, store.ErrExists):
		problem(w, http.StatusConflict, typeConflict, "it exists already")
	case errors.Is(err, store.ErrBusy), errors.Is(err, store.ErrStreamBusy), errors.Is(err, store.ErrClaimLost),
		errors.Is(err, store.ErrBuildGone): // spec 0016: the storage collector in the way; try again
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusServiceUnavailable, typeUnavailable, "busy: try again")
	case errors.Is(err, store.ErrConflict):
		problem(w, http.StatusPreconditionFailed, typePrecondition, "it changed: read it again")
	case errors.Is(err, tenants.ErrLastAdmin):
		problem(w, http.StatusConflict, typeConflict, "the tenant's last tenant-wide admin grant; a server administrator can remove it")
	case errors.Is(err, tenants.ErrIssuerFetch):
		problem(w, http.StatusBadRequest, typeInvalid, "the issuer's discovery document or JWKS could not be fetched or used")
	case errors.Is(err, store.ErrInvalid):
		problem(w, http.StatusBadRequest, typeInvalid, publicMessage(err))
	default:
		h.fail(w, err)
	}
}

// publicMessage is the typed message of an invalid-value error (the services word them for the
// caller: the value and the rule).
func publicMessage(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, store.ErrInvalid.Error()+": "); i >= 0 {
		msg = msg[i+len(store.ErrInvalid.Error())+2:]
	}
	if len(msg) > 300 {
		msg = strings.ToValidUTF8(msg[:300], "")
	}
	return msg
}

// ifMatch reads a required If-Match (428 without one): one strong ETag, or * (any version of an
// existing resource, returned as "").
func ifMatch(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := strings.TrimSpace(strings.Join(r.Header.Values("If-Match"), ","))
	switch {
	case v == "":
		problem(w, http.StatusPreconditionRequired, typeRequired, "this request needs If-Match with the resource's ETag")
		return "", false
	case v == "*":
		return "", true
	case strings.Contains(v, ","):
		problem(w, http.StatusBadRequest, typeInvalid, "If-Match takes one ETag")
		return "", false
	}
	return v, true
}

// page reads limit (1..500, default 100) and an opaque cursor.
func page(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	q := r.URL.Query()
	limit := 100
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 500 || s[0] < '0' || s[0] > '9' {
			problem(w, http.StatusBadRequest, typeInvalid, "limit is 1..500")
			return 0, "", false
		}
		limit = n
	}
	after := ""
	if s := q.Get("cursor"); s != "" {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			problem(w, http.StatusBadRequest, typeInvalid, "the cursor is not one this server returned")
			return 0, "", false
		}
		after = string(b)
	}
	return limit, after, true
}

// paged returns the items after the cursor key, at most limit, and the next cursor.
func paged[T any](items []T, key func(T) string, limit int, after string) ([]T, string) {
	items = slices.Clone(items)
	slices.SortStableFunc(items, func(a, b T) int { return strings.Compare(key(a), key(b)) })
	out := []T{}
	for _, it := range items {
		if after != "" && key(it) <= after {
			continue
		}
		if len(out) == limit {
			return out, base64.RawURLEncoding.EncodeToString([]byte(key(out[len(out)-1])))
		}
		out = append(out, it)
	}
	return out, ""
}

func list(key string, items any, next string) map[string]any {
	m := map[string]any{key: items}
	if next != "" {
		m["next"] = next
	}
	return m
}

// created answers 201 with Location; etag is the resource's (one with a version or an id), or "-".
func created(w http.ResponseWriter, r *http.Request, location string, v any, etag string) {
	w.Header().Set("Location", location)
	reply(w, r, http.StatusCreated, v, etag, noStore)
}

// noContent answers a deletion.
func noContent(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", noStore)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNoContent)
}

// duration formats a duration compactly: 24h, 90m, 1h30m.
func duration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func esc(s string) string { return url.PathEscape(s) }

// --- server identity ---

func (h *Handler) serverWhoami(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	var ps []string
	for k := range c.id.Principals {
		g := store.Grant{Kind: k.Kind, Value: k.Value, IssuerName: strings.TrimPrefix(k.IssuerID, "server:")}
		ps = append(ps, g.Principal())
	}
	sort.Strings(ps)
	reply(w, r, http.StatusOK, map[string]any{"issuer": c.id.Issuer.Name, "principals": nonNil(ps), "administrator": c.admin},
		"", "private, no-store")
}

// --- DuckDB versions ---

type versionOut struct {
	Name  string   `json:"name"`
	Kind  string   `json:"kind"`
	CAPIs []string `json:"c_api_maxima"`
}

func versionOf(v store.DuckDBVersion) versionOut {
	o := versionOut{Name: v.Name, Kind: v.Kind, CAPIs: []string{}}
	for _, c := range v.CAPIs {
		o.CAPIs = append(o.CAPIs, c.String())
	}
	return o
}

func (h *Handler) duckdbVersions(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	vs, err := h.o.Store.ListDuckDBVersions(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	out := []versionOut{}
	for _, v := range vs {
		out = append(out, versionOf(v))
	}
	answer(w, r, map[string]any{"duckdb_versions": out}, c.anonymous())
}

func (h *Handler) duckdbVersion(w http.ResponseWriter, r *http.Request, c caller, p params) {
	vs, err := h.o.Store.ListDuckDBVersions(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, v := range vs {
		if v.Name == p["v"] {
			answer(w, r, versionOf(v), c.anonymous())
			return
		}
	}
	notFound(w)
}

func (h *Handler) addDuckDBVersion(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	var in struct {
		Name  string   `json:"name"`
		Kind  string   `json:"kind"`
		CAPIs []string `json:"c_api_maxima"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	v, err := h.o.Tenants.AddVersion(r.Context(), a, in.Name, in.Kind, in.CAPIs)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	created(w, r, "/api/v1/duckdb-versions/"+esc(v.Name), versionOf(v), "-")
}

func (h *Handler) addCAPI(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		CAPI string `json:"c_api"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	v, err := h.o.Tenants.AddVersionCAPI(r.Context(), a, p["v"], in.CAPI)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, versionOf(v), "", noStore)
}

// --- tenants ---

type tenantOut struct {
	Name          string `json:"name"`
	DisplayName   string `json:"display_name,omitempty"`
	State         string `json:"state"`
	StorageDomain string `json:"storage_domain,omitempty"` // server administrators only
	CreatedAt     string `json:"created_at"`
	ETag          string `json:"etag"`
}

func versionTag(v int64) string { return `"v` + strconv.FormatInt(v, 10) + `"` }

func tenantOf(t store.Tenant, c caller) tenantOut {
	o := tenantOut{Name: t.Name, DisplayName: t.DisplayName, State: t.State, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		ETag: versionTag(t.Version)}
	if c.admin {
		o.StorageDomain = t.StorageDomain
	}
	return o
}

func (h *Handler) listTenants(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	ts, err := h.o.Tenants.ListTenants(r.Context(), a)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	ts, next := paged(ts, func(t store.Tenant) string { return t.Name }, limit, after)
	out := []tenantOut{}
	for _, t := range ts {
		out = append(out, tenantOf(t, c))
	}
	reply(w, r, http.StatusOK, list("tenants", out, next), "", noStore)
}

func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	var in struct {
		Name          string `json:"name"`
		DisplayName   string `json:"display_name"`
		StorageDomain string `json:"storage_domain"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	t, err := h.o.Tenants.CreateTenant(r.Context(), a, in.Name, in.DisplayName, in.StorageDomain)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(t.Name), tenantOf(t, c), versionTag(t.Version))
}

func (h *Handler) getTenant(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	t, err := h.o.Tenants.GetTenant(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, tenantOf(t, c), versionTag(t.Version), noStore)
}

func (h *Handler) suspend(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.setTenantState(w, r, c, p, store.TenantSuspended)
}

func (h *Handler) resume(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.setTenantState(w, r, c, p, store.TenantActive)
}

func (h *Handler) setTenantState(w http.ResponseWriter, r *http.Request, c caller, p params, state string) {
	tag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	var n int64 // 0 for *: any version
	if tag != "" {
		var err error
		n, err = strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(tag, `"v`), `"`), 10, 64)
		if err != nil || n <= 0 || versionTag(n) != tag {
			problem(w, http.StatusPreconditionFailed, typePrecondition, "it changed: read it again")
			return
		}
	}
	a, _ := c.actor()
	t, err := h.o.Tenants.SetTenantState(r.Context(), a, p["t"], state, n)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, tenantOf(t, c), versionTag(t.Version), noStore)
}

// --- audiences ---

func (h *Handler) listAudiences(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	auds, err := h.o.Auth.ListAudiences(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"audiences": nonNil(auds)}, "", noStore)
}

func (h *Handler) changeAudience(w http.ResponseWriter, r *http.Request, c caller, p params, add bool) {
	var in struct {
		Audience string `json:"audience"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	var err error
	if add {
		err = h.o.Auth.AddAudience(r.Context(), a, p["t"], in.Audience)
	} else {
		err = h.o.Auth.RemoveAudience(r.Context(), a, p["t"], in.Audience)
	}
	if err != nil {
		h.serviceErr(w, err, true)
		return
	}
	h.listAudiences(w, r, c, p)
}

func (h *Handler) addAudience(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.changeAudience(w, r, c, p, true)
}

func (h *Handler) removeAudience(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.changeAudience(w, r, c, p, false)
}

// --- issuers ---

type issuerJSON struct {
	Name             string            `json:"name"`
	URL              string            `json:"url"`
	JWKSURI          string            `json:"jwks_uri,omitempty"`
	Algorithms       []string          `json:"algorithms,omitempty"`
	RequiredClaims   map[string]string `json:"required_claims,omitempty"`
	RolesClaim       []string          `json:"roles_claim,omitempty"`
	GroupsClaim      []string          `json:"groups_claim,omitempty"`
	ClientClaim      []string          `json:"client_claim,omitempty"`
	MaxTokenLifetime string            `json:"max_token_lifetime,omitempty"` // a Go duration: 24h
	CreatedAt        string            `json:"created_at,omitempty"`
	CreatedBy        string            `json:"created_by,omitempty"`
	ETag             string            `json:"etag,omitempty"`
}

func issuerTag(is store.Issuer) string { return `"` + is.ID + `"` }

// createdBy shows who made a record: a server administrator's identity (over the API, or the CLI's
// OS user) only to server administrators (on Enterest it is the operator's staff).
func createdBy(by string, c caller) string {
	if !c.admin && (strings.HasPrefix(by, string(authz.ActorServer)+":") || strings.HasPrefix(by, string(authz.ActorOS)+":")) {
		return string(authz.ActorServer)
	}
	return by
}

func issuerOf(is store.Issuer, c caller) issuerJSON {
	return issuerJSON{Name: is.Name, URL: is.URL, JWKSURI: is.JWKSURI, Algorithms: is.Algorithms, RequiredClaims: is.RequiredClaims,
		RolesClaim: is.RolesClaim, GroupsClaim: is.GroupsClaim, ClientClaim: is.ClientClaim,
		MaxTokenLifetime: duration(is.MaxTokenLifetime), CreatedAt: is.CreatedAt.UTC().Format(time.RFC3339),
		CreatedBy: createdBy(is.CreatedBy, c), ETag: issuerTag(is)}
}

func (h *Handler) tenantIssuers(w http.ResponseWriter, r *http.Request, c caller, p params) ([]store.Issuer, bool) {
	a, _ := c.actor()
	iss, err := h.o.Auth.ListIssuers(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return nil, false
	}
	return iss, true
}

func (h *Handler) listIssuers(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	iss, ok := h.tenantIssuers(w, r, c, p)
	if !ok {
		return
	}
	iss, next := paged(iss, func(is store.Issuer) string { return is.Name }, limit, after)
	out := []issuerJSON{}
	for _, is := range iss {
		out = append(out, issuerOf(is, c))
	}
	reply(w, r, http.StatusOK, list("issuers", out, next), "", noStore)
}

func (h *Handler) addIssuer(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in issuerJSON
	if !body(w, r, &in) {
		return
	}
	if in.CreatedAt != "" || in.CreatedBy != "" || in.ETag != "" {
		problem(w, http.StatusBadRequest, typeInvalid, "created_at, created_by and etag are not set by the caller")
		return
	}
	is := store.Issuer{Name: in.Name, URL: in.URL, JWKSURI: in.JWKSURI, Algorithms: in.Algorithms, RequiredClaims: in.RequiredClaims,
		RolesClaim: in.RolesClaim, GroupsClaim: in.GroupsClaim, ClientClaim: in.ClientClaim}
	if in.MaxTokenLifetime != "" {
		d, err := time.ParseDuration(in.MaxTokenLifetime)
		if err != nil {
			problem(w, http.StatusBadRequest, typeInvalid, "max_token_lifetime is a duration such as 24h")
			return
		}
		is.MaxTokenLifetime = d
	}
	// discovery and the JWKS fetch run one at a time per tenant (spec 0007)
	h.mu.Lock()
	busy := h.egress[c.tenant.ID]
	if !busy {
		h.egress[c.tenant.ID] = true
	}
	h.mu.Unlock()
	if busy {
		w.Header().Set("Retry-After", "5")
		problem(w, http.StatusTooManyRequests, typeTooMany, "an issuer is being added to this tenant")
		return
	}
	defer func() {
		h.mu.Lock()
		delete(h.egress, c.tenant.ID)
		h.mu.Unlock()
	}()
	a, _ := c.actor()
	out, err := h.o.Auth.AddIssuer(r.Context(), a, p["t"], is)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/issuers/"+esc(out.Name), issuerOf(out, c), issuerTag(out))
}

func (h *Handler) getIssuer(w http.ResponseWriter, r *http.Request, c caller, p params) {
	iss, ok := h.tenantIssuers(w, r, c, p)
	if !ok {
		return
	}
	for _, is := range iss {
		if is.Name == p["name"] {
			reply(w, r, http.StatusOK, issuerOf(is, c), issuerTag(is), noStore)
			return
		}
	}
	notFound(w)
}

func (h *Handler) removeIssuer(w http.ResponseWriter, r *http.Request, c caller, p params) {
	tag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(tag, `"`), `"`) // "" for *: any record under the name
	if tag != "" && (id == "" || `"`+id+`"` != tag) {
		problem(w, http.StatusPreconditionFailed, typePrecondition, "it changed: read it again")
		return
	}
	a, _ := c.actor()
	if err := h.o.Auth.RemoveIssuer(r.Context(), a, p["t"], p["name"], id); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}

// --- grants ---

type grantJSON struct {
	ID        string   `json:"id"`
	Principal string   `json:"principal"`
	Verbs     []string `json:"verbs"`
	Channel   string   `json:"channel,omitempty"`
	Extension string   `json:"extension,omitempty"`
	CreatedAt string   `json:"created_at"`
	CreatedBy string   `json:"created_by"`
}

func grantOf(g store.Grant, c caller) grantJSON {
	return grantJSON{ID: g.ID, Principal: g.Principal(), Verbs: g.Verbs, Channel: g.ChannelName, Extension: g.Extension,
		CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339), CreatedBy: createdBy(g.CreatedBy, c)}
}

// grantKey orders grants as the store lists them: (created_at, id).
func grantKey(g store.Grant) string {
	return g.CreatedAt.UTC().Format("20060102T150405.000000000") + "|" + g.ID
}

func (h *Handler) tenantGrants(w http.ResponseWriter, r *http.Request, c caller, p params) ([]store.Grant, bool) {
	a, _ := c.actor()
	gs, err := h.o.Auth.ListGrants(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return nil, false
	}
	sort.SliceStable(gs, func(i, j int) bool { return grantKey(gs[i]) < grantKey(gs[j]) })
	return gs, true
}

func (h *Handler) listGrants(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	gs, ok := h.tenantGrants(w, r, c, p)
	if !ok {
		return
	}
	if pr := r.URL.Query().Get("principal"); pr != "" {
		var kept []store.Grant
		for _, g := range gs {
			if g.Principal() == pr {
				kept = append(kept, g)
			}
		}
		gs = kept
	}
	gs, next := paged(gs, grantKey, limit, after)
	out := []grantJSON{}
	for _, g := range gs {
		out = append(out, grantOf(g, c))
	}
	reply(w, r, http.StatusOK, list("grants", out, next), "", noStore)
}

func (h *Handler) addGrant(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Principal string   `json:"principal"`
		Verbs     []string `json:"verbs"`
		Channel   string   `json:"channel"`
		Extension string   `json:"extension"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	g, err := h.o.Auth.AddGrant(r.Context(), a, p["t"], in.Principal, in.Verbs, in.Channel, in.Extension)
	if err != nil {
		h.serviceErr(w, err, true)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/grants/"+esc(g.ID), grantOf(g, c), "-")
}

func (h *Handler) getGrant(w http.ResponseWriter, r *http.Request, c caller, p params) {
	gs, ok := h.tenantGrants(w, r, c, p)
	if !ok {
		return
	}
	for _, g := range gs {
		if g.ID == p["id"] {
			reply(w, r, http.StatusOK, grantOf(g, c), "", noStore)
			return
		}
	}
	notFound(w)
}

func (h *Handler) removeGrant(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Auth.RemoveGrant(r.Context(), a, p["t"], p["id"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}
