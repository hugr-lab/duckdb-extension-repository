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
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
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
		if errors.Is(err, errBodyNotSigned) {
			problem(w, http.StatusBadRequest, typeInvalid, "the body is not the signed one: its SHA-256 is not the statement's")
		} else if errors.As(err, &tooLarge) {
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
	case errors.Is(err, tenants.ErrGroupGranted):
		problem(w, http.StatusConflict, typeConflict, "a grant names the extension group: revoke its grants first")
	case errors.Is(err, tenants.ErrLastAdmin):
		problem(w, http.StatusConflict, typeConflict, "the tenant's last tenant-wide admin grant; a server administrator can remove it")
	case errors.As(err, new(*tenants.Refusal)):
		var rf *tenants.Refusal
		errors.As(err, &rf)
		problem(w, http.StatusConflict, typeConflict, rf.Reason)
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

// namePrefix reads ?prefix=, an extension name's beginning (spec 0015 phase 1b): at most 64 bytes of
// the name grammar's characters, so it never needs escaping in a log or a cursor.
func namePrefix(w http.ResponseWriter, r *http.Request) (string, bool) {
	s := r.URL.Query().Get("prefix")
	nameChar := func(c rune) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
	}
	if len(s) > 64 || strings.IndexFunc(s, func(c rune) bool { return !nameChar(c) }) >= 0 {
		problem(w, http.StatusBadRequest, typeInvalid, "prefix is at most 64 letters, digits, '_', '-' or '.'")
		return "", false
	}
	return s, true
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
	reply(w, r, http.StatusOK, map[string]any{"issuer": c.id.Issuer.Name, "principals": nonNil(ps), "administrator": c.admin,
		"license_manager": c.licenseManager, "distribution_admin": c.distribution}, "", "private, no-store")
}

// --- DuckDB versions ---

type versionOut struct {
	Name  string   `json:"name"`
	Kind  string   `json:"kind"`
	CAPIs []string `json:"c_api_maxima"`
}

// versionAdded is a registration's answer: the version, and the core extensions it reserved that
// were not reserved before (spec 0030).
type versionAdded struct {
	versionOut
	CoreAdded []string `json:"core_extensions_added"`
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
		Name       string   `json:"name"`
		Kind       string   `json:"kind"`
		CAPIs      []string `json:"c_api_maxima"`
		Core       []string `json:"core_extensions"`
		CoreCommit string   `json:"core_extensions_commit"`
		Force      bool     `json:"force"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	v, added, err := h.o.Tenants.AddVersion(r.Context(), a, in.Name, in.Kind, in.CAPIs,
		tenants.CoreExtensions{Names: in.Core, Commit: in.CoreCommit, Force: in.Force})
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	if added == nil {
		added = []string{}
	}
	created(w, r, "/api/v1/duckdb-versions/"+esc(v.Name), versionAdded{versionOf(v), added}, "-")
}

// addCoreExtensions reserves more core extensions of a registered version (spec 0030).
func (h *Handler) addCoreExtensions(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Core       []string `json:"core_extensions"`
		CoreCommit string   `json:"core_extensions_commit"`
		Force      bool     `json:"force"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	added, err := h.o.Tenants.AddVersionCore(r.Context(), a, p["v"], tenants.CoreExtensions{Names: in.Core, Commit: in.CoreCommit, Force: in.Force})
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	if added == nil {
		added = []string{}
	}
	reply(w, r, http.StatusOK, map[string]any{"name": p["v"], "core_extensions_added": added}, "", noStore)
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
	Name          string       `json:"name"`
	DisplayName   string       `json:"display_name,omitempty"`
	State         string       `json:"state"`                    // active, suspended, deleting or deleted (spec 0019)
	StorageDomain string       `json:"storage_domain,omitempty"` // server administrators only
	CreatedAt     string       `json:"created_at"`
	SuspendedAt   string       `json:"suspended_at,omitempty"`
	DeletableAt   string       `json:"deletable_at,omitempty"` // server administrators only
	Deletion      *deletionOut `json:"deletion,omitempty"`     // server administrators only
	HolderID      string       `json:"holder_id,omitempty"`    // the holder its licences must name (spec 0028)
	ETag          string       `json:"etag"`
}

type deletionOut struct {
	StartedAt     string              `json:"started_at"`
	StartedBy     string              `json:"started_by"`
	Passes        int64               `json:"passes"`
	Table         string              `json:"table,omitempty"`
	DeletedRows   int64               `json:"deleted_rows"`
	LastError     string              `json:"last_error,omitempty"`
	NextAttemptAt string              `json:"next_attempt_at,omitempty"`
	FinishedAt    string              `json:"finished_at,omitempty"`
	Keys          []store.DeletionKey `json:"keys"`
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// withDeletion adds what a server administrator sees of a tenant's deletion (spec 0019).
func (h *Handler) withDeletion(o tenantOut, t store.Tenant, d *store.Deletion, c caller) tenantOut {
	if !c.admin {
		return o
	}
	o.DeletableAt = rfc3339(tenants.DeletableAt(t, h.o.MinSuspended))
	if d != nil {
		o.Deletion = &deletionOut{StartedAt: rfc3339(d.StartedAt), StartedBy: d.StartedBy, Passes: d.Passes, Table: d.Table,
			DeletedRows: d.DeletedRows, LastError: d.LastError, FinishedAt: rfc3339(d.FinishedAt), Keys: d.Keys}
		if d.FinishedAt.IsZero() {
			o.Deletion.NextAttemptAt = rfc3339(d.NextAttemptAt)
		}
	}
	return o
}

func versionTag(v int64) string { return `"v` + strconv.FormatInt(v, 10) + `"` }

func tenantOf(t store.Tenant, c caller) tenantOut {
	o := tenantOut{Name: t.Name, DisplayName: t.DisplayName, State: t.Phase(), CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		SuspendedAt: rfc3339(t.SuspendedAt), HolderID: t.HolderID, ETag: versionTag(t.Version)}
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
	var phases []string
	if q := r.URL.Query().Get("state"); q != "" {
		phases = strings.Split(q, ",")
	}
	ts, err := h.o.Tenants.ListTenants(r.Context(), a, phases...)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	ts, next := paged(ts, func(t store.Tenant) string { return t.Name }, limit, after)
	out := []tenantOut{}
	for _, t := range ts {
		out = append(out, h.withDeletion(tenantOf(t, c), t, nil, c))
	}
	reply(w, r, http.StatusOK, list("tenants", out, next), "", noStore)
}

func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	var in struct {
		Name          string `json:"name"`
		DisplayName   string `json:"display_name"`
		StorageDomain string `json:"storage_domain"`
		HolderID      string `json:"holder_id"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	t, err := h.o.Tenants.CreateTenant(r.Context(), a, in.Name, in.DisplayName, in.StorageDomain, in.HolderID)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(t.Name), tenantOf(t, c), versionTag(t.Version))
}

func (h *Handler) getTenant(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if c.admin { // spec 0019: any phase, with the deletion's record
		t, d, err := h.o.Tenants.Show(r.Context(), a, p["t"])
		if err != nil {
			h.serviceErr(w, err, false)
			return
		}
		reply(w, r, http.StatusOK, h.withDeletion(tenantOf(t, c), t, d, c), versionTag(t.Version), noStore)
		return
	}
	t, err := h.o.Tenants.GetTenant(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, tenantOf(t, c), versionTag(t.Version), noStore)
}

// deleteTenant starts a suspended tenant's deletion (spec 0019): an exact If-Match, 202 while the
// reaper works.
func (h *Handler) deleteTenant(w http.ResponseWriter, r *http.Request, c caller, p params) {
	tag, ok := ifMatch(w, r)
	if !ok {
		return
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(tag, `"v`), `"`), 10, 64)
	if tag == "" {
		problem(w, http.StatusPreconditionRequired, typeRequired, "deleting a tenant needs If-Match with its ETag, not *")
		return
	}
	if err != nil || n <= 0 || versionTag(n) != tag {
		problem(w, http.StatusPreconditionFailed, typePrecondition, "it changed: read it again")
		return
	}
	a, _ := c.actor()
	t, d, err := h.o.Tenants.Delete(r.Context(), a, p["t"], n, h.o.MinSuspended)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusAccepted, h.withDeletion(tenantOf(t, c), t, &d, c), versionTag(t.Version), noStore)
}

func (h *Handler) suspend(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.setTenantState(w, r, c, p, store.TenantSuspended)
}

func (h *Handler) resume(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.setTenantState(w, r, c, p, store.TenantActive)
}

// tenantVersion reads a tenant's required If-Match: its version, or 0 for * (any version).
func tenantVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	tag, ok := ifMatch(w, r)
	if !ok {
		return 0, false
	}
	if tag == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(tag, `"v`), `"`), 10, 64)
	if err != nil || n <= 0 || versionTag(n) != tag {
		problem(w, http.StatusPreconditionFailed, typePrecondition, "it changed: read it again")
		return 0, false
	}
	return n, true
}

func (h *Handler) setTenantState(w http.ResponseWriter, r *http.Request, c caller, p params, state string) {
	n, ok := tenantVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	t, err := h.o.Tenants.SetTenantState(r.Context(), a, p["t"], state, n)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, h.withDeletion(tenantOf(t, c), t, nil, c), versionTag(t.Version), noStore)
}

// setTenantHolder sets the holder a tenant's licences must name (spec 0028): server administrators
// only, If-Match on the tenant; 409 while it holds a licence of another holder.
func (h *Handler) setTenantHolder(w http.ResponseWriter, r *http.Request, c caller, p params) {
	n, ok := tenantVersion(w, r)
	if !ok {
		return
	}
	var in struct {
		HolderID string `json:"holder_id"`
	}
	if !body(w, r, &in) {
		return
	}
	if in.HolderID == "" {
		problem(w, http.StatusBadRequest, typeInvalid, "holder_id is required: 1 to 128 bytes, as a licence's sub (DELETE clears it)")
		return
	}
	h.tenantHolder(w, r, c, p, in.HolderID, n)
}

// clearTenantHolder clears a tenant's expected holder (spec 0028).
func (h *Handler) clearTenantHolder(w http.ResponseWriter, r *http.Request, c caller, p params) {
	n, ok := tenantVersion(w, r)
	if !ok {
		return
	}
	h.tenantHolder(w, r, c, p, "", n)
}

func (h *Handler) tenantHolder(w http.ResponseWriter, r *http.Request, c caller, p params, holder string, n int64) {
	a, _ := c.actor()
	t, err := h.o.Tenants.SetTenantHolder(r.Context(), a, p["t"], holder, n)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, h.withDeletion(tenantOf(t, c), t, nil, c), versionTag(t.Version), noStore)
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
// OS user) only to server administrators (on Enterest it is the operator's staff); a licence
// manager's (spec 0029) shows as its kind alone, the same way.
func createdBy(by string, c caller) string {
	switch {
	case c.admin: // sees who
	case strings.HasPrefix(by, string(authz.ActorServer)+":") || strings.HasPrefix(by, string(authz.ActorOS)+":"):
		return string(authz.ActorServer)
	case strings.HasPrefix(by, string(authz.ActorLicenseManager)+":"):
		return string(authz.ActorLicenseManager)
	case strings.HasPrefix(by, string(authz.ActorDistributionAdmin)+":"):
		return string(authz.ActorDistributionAdmin)
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
	id, ok := issuerIfMatch(w, r) // "" for *: any record under the name
	if !ok {
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
	ID             string   `json:"id"`
	Principal      string   `json:"principal"`
	Verbs          []string `json:"verbs"`
	Channel        string   `json:"channel,omitempty"`
	Extension      string   `json:"extension,omitempty"`
	ExtensionGroup string   `json:"extension_group,omitempty"`
	CreatedAt      string   `json:"created_at"`
	CreatedBy      string   `json:"created_by"`
	// with ?extension= (spec 0017): how the grant reaches the name, and the verbs that apply to it
	Via              string   `json:"via,omitempty"`
	VerbsOnExtension []string `json:"verbs_on_extension,omitempty"`
}

func grantOf(g store.Grant, c caller) grantJSON {
	return grantJSON{ID: g.ID, Principal: g.Principal(), Verbs: g.Verbs, Channel: g.ChannelName, Extension: g.Extension,
		ExtensionGroup: g.ExtensionGroupName, CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339), CreatedBy: createdBy(g.CreatedBy, c)}
}

// reaching is how a grant reaches an extension and the verbs it gives there (spec 0017).
func reaching(g store.Grant, tenant, ext string, isReserved bool) (string, []string) {
	return authz.Reaching(g, tenant, ext, isReserved)
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
	q := r.URL.Query()
	pr, group, ext := q.Get("principal"), q.Get("extension_group"), q.Get("extension")
	if group != "" && store.ValidGroupName(group) != nil || ext != "" && release.ValidName(ext) != nil {
		problem(w, http.StatusBadRequest, typeInvalid, "extension_group or extension is not a valid name")
		return
	}
	isReserved := false
	if ext != "" {
		kind, err := h.o.Store.ReservedFor(r.Context(), c.tenant.Name, ext) // spec 0030: DuckDB's, the tenant's, its upstreams'
		if err != nil {
			h.fail(w, err)
			return
		}
		isReserved = kind != ""
	}
	var kept []store.Grant
	for _, g := range gs {
		if pr != "" && g.Principal() != pr || group != "" && g.ExtensionGroupName != group {
			continue
		}
		if ext != "" {
			if _, verbs := reaching(g, c.tenant.Name, ext, isReserved); len(verbs) == 0 {
				continue
			}
		}
		kept = append(kept, g)
	}
	kept, next := paged(kept, grantKey, limit, after)
	out := []grantJSON{}
	for _, g := range kept {
		o := grantOf(g, c)
		if ext != "" {
			o.Via, o.VerbsOnExtension = reaching(g, c.tenant.Name, ext, isReserved)
		}
		out = append(out, o)
	}
	reply(w, r, http.StatusOK, list("grants", out, next), "", noStore)
}

// grantPrincipals answers the tenant's principals with their grant counts (spec 0015 phase 2a), by
// principal, paged, with a prefix of the principal.
func (h *Handler) grantPrincipals(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if len(prefix) > 300 {
		problem(w, http.StatusBadRequest, typeInvalid, "prefix is at most 300 bytes")
		return
	}
	gs, ok := h.tenantGrants(w, r, c, p)
	if !ok {
		return
	}
	type row struct {
		Principal string `json:"principal"`
		Grants    int    `json:"grants"`
	}
	counts := map[string]int{}
	for _, g := range gs {
		if pr := g.Principal(); strings.HasPrefix(pr, prefix) {
			counts[pr]++
		}
	}
	rows := make([]row, 0, len(counts))
	for pr, n := range counts {
		rows = append(rows, row{pr, n})
	}
	rows, next := paged(rows, func(x row) string { return x.Principal }, limit, after)
	reply(w, r, http.StatusOK, list("principals", rows, next), "", noStore)
}

func (h *Handler) addGrant(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Principal      string   `json:"principal"`
		Verbs          []string `json:"verbs"`
		Channel        string   `json:"channel"`
		Extension      string   `json:"extension"`
		ExtensionGroup string   `json:"extension_group"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	g, err := h.o.Auth.AddGrant(r.Context(), a, p["t"], in.Principal, in.Verbs,
		tenants.GrantOn{Channel: in.Channel, Extension: in.Extension, ExtensionGroup: in.ExtensionGroup})
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

// --- reserved names (spec 0030) ---

type reservedOut struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	AddedAt string   `json:"added_at"`
	Sources []string `json:"sources"`
}

func (h *Handler) listReserved(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	a, _ := c.actor()
	ns, err := h.o.Tenants.ListReserved(r.Context(), a, r.URL.Query().Get("name"))
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []reservedOut{}
	for _, n := range ns {
		out = append(out, reservedOut{Name: n.Name, Kind: n.Kind, AddedAt: n.AddedAt.UTC().Format(time.RFC3339), Sources: n.Sources})
	}
	reply(w, r, http.StatusOK, map[string]any{"names": out}, "", noStore)
}

func (h *Handler) addReserved(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	var in struct {
		Names []string `json:"names"`
		Kind  string   `json:"kind"`
		Force bool     `json:"force"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	res, err := h.o.Tenants.Reserve(r.Context(), a, in.Names, in.Kind, in.Force)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	if res.Added == nil {
		res.Added = []string{}
	}
	reply(w, r, http.StatusOK, map[string]any{"added": res.Added, "tenants": res.Tenants}, "", noStore)
}

func (h *Handler) removeReserved(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Tenants.Unreserve(r.Context(), a, p["name"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}

// --- names reserved in a tenant (spec 0030) ---

type tenantReservedOut struct {
	Name    string `json:"name"`
	Holder  string `json:"holder"`
	AddedAt string `json:"added_at"`
	AddedBy string `json:"added_by"`
}

type liveReleaseOut struct {
	Channel  string `json:"channel"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	State    string `json:"state"`
	Origin   string `json:"origin"`
}

func tenantReservedOf(r store.TenantReservedName, c caller) tenantReservedOut {
	return tenantReservedOut{Name: r.Name, Holder: r.Holder, AddedAt: rfc3339(r.AddedAt), AddedBy: createdBy(r.AddedBy, c)}
}

func (h *Handler) listTenantReserved(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	rs, err := h.o.Tenants.ListInTenant(r.Context(), a, p["t"], "")
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []tenantReservedOut{}
	for _, x := range rs {
		out = append(out, tenantReservedOf(x, c))
	}
	reply(w, r, http.StatusOK, map[string]any{"reserved_names": out}, "", noStore)
}

func (h *Handler) getTenantReserved(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	rs, err := h.o.Tenants.ListInTenant(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	if len(rs) == 0 {
		notFound(w)
		return
	}
	reply(w, r, http.StatusOK, tenantReservedOf(rs[0], c), "", noStore)
}

func (h *Handler) reserveInTenant(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Holder string `json:"holder"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	res, err := h.o.Tenants.ReserveInTenant(r.Context(), a, p["t"], p["name"], in.Holder)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	rels := []liveReleaseOut{}
	for _, x := range res.Releases {
		rels = append(rels, liveReleaseOut(x))
	}
	out := map[string]any{"name": res.Name, "holder": res.Holder, "added_at": rfc3339(res.AddedAt), "added_by": createdBy(res.AddedBy, c),
		"changed": res.Changed, "releases": rels}
	if res.Previous != "" && res.Previous != res.Holder {
		out["previous_holder"] = res.Previous
	}
	reply(w, r, http.StatusOK, out, "", noStore)
}

func (h *Handler) releaseInTenant(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Tenants.ReleaseInTenant(r.Context(), a, p["t"], p["name"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}
