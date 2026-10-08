// Package api is /api/v1 (spec 0007): the index of what a channel serves to the caller, and
// management over the services kista admin uses. kista serve hands /api/ to it on https requests
// only.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Version is the API version reported by /api/v1/info.
const Version = "1"

// Options configure a Handler.
type Options struct {
	Store     *store.Store
	Snapshots *release.Snapshots
	Auths     *auth.TenantAuths
	Verifier  *auth.Verifier
	PublicURL string // canonical audiences: <public_url>/<tenant>
	Rate      float64
	Burst     int
	Log       *slog.Logger
	// KistaVersion is reported by /api/v1/info.
	KistaVersion string

	// Server verifies server tokens (nil: none are accepted).
	Server *auth.Server
	// Authz, Tenants and Auth are management's authorizer and services (with that authorizer); nil
	// Tenants or Auth: no management routes.
	Authz            authz.Authorizer
	Tenants          *tenants.Service
	Auth             *tenants.AuthAdmin
	Keys             *keys.Service    // phase 3
	Releases         *release.Service // phase 3
	AdminTokenMaxAge time.Duration    // default 1h
	Now              func() time.Time
}

// Handler serves /api/v1.
type Handler struct {
	o        Options
	limiter  *limiter
	failures *auth.FailureLog

	mu     sync.Mutex
	egress map[string]bool // tenant id -> an issuer add (discovery, JWKS) is running
}

// New returns a handler.
func New(o Options) *Handler {
	if o.Rate == 0 {
		o.Rate, o.Burst = 20, 40
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.AdminTokenMaxAge == 0 {
		o.AdminTokenMaxAge = time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Handler{o: o, limiter: newLimiter(o.Rate, o.Burst), failures: &auth.FailureLog{Log: o.Log}, egress: map[string]bool{}}
}

type ctxKey int

const clientKey ctxKey = 0

// WithClient records the request's client address (kista serve computes it, spec 0006).
func WithClient(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, clientKey, addr)
}

func client(r *http.Request) string {
	if c, ok := r.Context().Value(clientKey).(string); ok {
		return c
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// --- problems ---

// problem types: a fixed set.
const (
	typeUnauthorized = "unauthorized"
	typeNotFound     = "not-found"
	typeMethod       = "method-not-allowed"
	typeInvalid      = "invalid"
	typeConflict     = "conflict"
	typePrecondition = "precondition-failed"
	typeRequired     = "precondition-required"
	typeTooLarge     = "too-large"
	typeMediaType    = "unsupported-media-type"
	typeTooMany      = "too-many-requests"
	typeUnavailable  = "unavailable"
)

var titles = map[string]string{typeUnauthorized: "Unauthorized", typeNotFound: "Not Found", typeMethod: "Method Not Allowed",
	typeInvalid: "Invalid", typeConflict: "Conflict", typePrecondition: "Precondition Failed",
	typeRequired: "Precondition Required", typeTooLarge: "Content Too Large", typeMediaType: "Unsupported Media Type",
	typeTooMany: "Too Many Requests", typeUnavailable: "Service Unavailable"}

// problem writes application/problem+json. 401 and 404 have constant bytes.
func problem(w http.ResponseWriter, status int, typ, detail string) {
	body := map[string]any{"type": "urn:kista:problem:" + typ, "title": titles[typ], "status": status}
	if detail != "" && status != http.StatusUnauthorized && status != http.StatusNotFound {
		body["detail"] = detail
	}
	b, _ := json.Marshal(body)
	hd := w.Header()
	hd.Del("ETag")
	hd.Set("Content-Type", "application/problem+json")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Vary", "Authorization")
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	if status == http.StatusUnauthorized {
		hd.Set("WWW-Authenticate", `Bearer realm="kista"`)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func notFound(w http.ResponseWriter)     { problem(w, http.StatusNotFound, typeNotFound, "") }
func unauthorized(w http.ResponseWriter) { problem(w, http.StatusUnauthorized, typeUnauthorized, "") }

// answer writes an index answer: JSON with an ETag over its bytes (304 on a match).
func answer(w http.ResponseWriter, r *http.Request, v any, anonymous bool) {
	cc := "private, no-cache"
	if anonymous {
		cc = "public, no-cache"
	}
	reply(w, r, http.StatusOK, v, "", cc)
}

// reply writes JSON with an ETag (the given one, "-" for none, or one over the bytes) and 304 on a
// matching If-None-Match for a GET.
func reply(w http.ResponseWriter, r *http.Request, status int, v any, etag, cacheControl string) {
	b, err := json.Marshal(v)
	if err != nil {
		problem(w, http.StatusServiceUnavailable, typeUnavailable, "")
		return
	}
	switch etag {
	case "":
		sum := sha256.Sum256(b)
		etag = `"` + hex.EncodeToString(sum[:16]) + `"`
	case "-":
		etag = ""
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("X-Content-Type-Options", "nosniff")
	if etag != "" {
		hd.Set("ETag", etag)
	}
	hd.Set("Vary", "Authorization")
	hd.Set("Cache-Control", cacheControl)
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		for _, t := range strings.Split(r.Header.Get("If-None-Match"), ",") {
			if t = strings.TrimSpace(t); etag != "" && (t == "*" || strings.TrimPrefix(t, "W/") == etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

// --- rate limiting ---

// maxClients bounds the limiter's table.
const maxClients = 100000

// limiter is a token bucket per client (IPv6 by /64), bounded.
type limiter struct {
	rate  float64
	burst float64
	mu    sync.Mutex
	b     map[string]*bucket
	swept time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), b: map[string]*bucket{}}
}

func clientKeyOf(addr string) string {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

func (l *limiter) allow(addr string) bool {
	k := clientKeyOf(addr)
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.b[k]
	if b == nil {
		if len(l.b) >= maxClients && !l.sweep(now) {
			return false // the table is full of clients still limited: a new one waits
		}
		b = &bucket{tokens: l.burst, last: now}
		l.b[k] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops the buckets that have refilled (the same as no bucket), reporting whether any went; at
// most once a second, so a full table of limited clients does not make every new one a scan.
func (l *limiter) sweep(now time.Time) bool {
	if now.Sub(l.swept) < time.Second {
		return false
	}
	l.swept = now
	n := len(l.b)
	for k, b := range l.b {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.b, k)
		}
	}
	return len(l.b) < n
}

// retryAfter is the whole seconds until a token is back.
func (l *limiter) retryAfter() string {
	return strconv.Itoa(max(1, int(math.Ceil(1/l.rate))))
}

// --- callers ---

// caller is who asks: anonymous, a server token (an administrator's or not), or a tenant token's
// principals.
type caller struct {
	tenant     store.Tenant // on tenant routes
	ta         store.TenantAuth
	principals auth.Principals // a tenant token's
	id         auth.Identity   // a verified token's identity
	verified   bool            // a valid token (server or tenant)
	server     bool            // a valid server token
	admin      bool            // a server administrator's
}

func (c caller) anonymous() bool { return !c.verified }

// actor is the caller as the services see it; false for a caller with no rights to act on anything
// (anonymous, a server token that is not an administrator's).
func (c caller) actor() (authz.Actor, bool) {
	switch {
	case c.admin:
		return authz.Actor{Kind: authz.ActorServer, ID: c.id.Issuer.Name + "|" + c.id.Who()}, true
	case c.principals != nil:
		return authz.Actor{Kind: authz.ActorPrincipal, ID: c.tenant.Name + "/" + c.id.Issuer.ID + "|" + c.id.Who(),
			Tenant: c.tenant.Name, Principals: c.principals}, true
	}
	return authz.Actor{}, false
}

// bearer returns the request's token from exactly one Authorization header, and whether one was
// sent at all.
func bearer(r *http.Request) (string, bool) {
	vals := r.Header.Values("Authorization")
	if len(vals) == 0 {
		return "", false
	}
	if len(vals) != 1 {
		return "", true
	}
	sch, tok, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(sch, "Bearer") {
		return "", true
	}
	return strings.TrimSpace(tok), true
}

var errToken = errors.New("api: the token is not valid")

// identifyServer verifies a token carrying a server audience against the server issuers only; it
// reports false for any other token (or none).
func (h *Handler) identifyServer(ctx context.Context, tok string) (caller, bool, error) {
	if tok == "" || !h.o.Server.IsServerToken(tok) {
		return caller{}, false, nil
	}
	id, admin, err := h.o.Server.Verify(ctx, tok)
	if err != nil {
		h.failures.Record(store.Tenant{Name: "(server)"}, "api", err)
		return caller{}, true, errToken
	}
	return caller{id: id, verified: true, server: true, admin: admin}, true, nil
}

// identifyTenant verifies a tenant token against the tenant (spec 0006).
func (h *Handler) identifyTenant(ctx context.Context, t store.Tenant, tok string) (caller, error) {
	c := caller{tenant: t}
	if tok == "" || h.o.Verifier == nil {
		return c, errToken
	}
	ta, err := h.o.Auths.Get(ctx, t)
	if err != nil {
		return c, err
	}
	ta, canonical := auth.ForTenant(ta, h.o.PublicURL, t.Name)
	id, err := h.o.Verifier.VerifyIdentity(ctx, ta, canonical, tok)
	if err != nil {
		h.failures.Record(t, "api", err)
		return c, errToken
	}
	c.principals, c.ta, c.id, c.verified = id.Principals, ta, id, true
	return c, nil
}

// --- routing ---

// access says who may call a route; decided from the path before anything else.
type access int

const (
	public      access = iota // anyone; a tenant route identifies a token for the view, others ignore one
	serverToken               // a valid server token
	serverAdmin               // a server administrator
	pathAdmin                 // admin on the path's resource (tenant, channel {c}, extension {ext}), or a server administrator
)

type handler func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params)

type route struct {
	pattern string // segments; {x} matches any one
	methods map[string]rule
}

// rule is one method of a route.
type rule struct {
	access access
	manage bool // management: a fresh token; for a write, a named writer
	body   bool // the request has a JSON body; any other request has none
	handle handler
}

type params map[string]string

func (rt route) match(segs []string) (params, bool) {
	ps := strings.Split(rt.pattern, "/")
	if len(ps) != len(segs) {
		return nil, false
	}
	p := params{}
	for i, s := range ps {
		if strings.HasPrefix(s, "{") {
			p[strings.Trim(s, "{}")] = segs[i]
		} else if s != segs[i] {
			return nil, false
		}
	}
	return p, true
}

var routes []route

func init() {
	idx := func(f handler) map[string]rule { return map[string]rule{http.MethodGet: {access: public, handle: f}} }
	m := func(a access, f handler) rule { return rule{access: a, manage: true, handle: f} }
	mb := func(a access, f handler) rule { return rule{access: a, manage: true, body: true, handle: f} }
	routes = []route{
		{"info", idx((*Handler).info)},
		{"whoami", map[string]rule{http.MethodGet: {access: serverToken, handle: (*Handler).serverWhoami}}},
		{"duckdb-versions", map[string]rule{http.MethodGet: {access: public, handle: (*Handler).duckdbVersions},
			http.MethodPost: mb(serverAdmin, (*Handler).addDuckDBVersion)}},
		{"duckdb-versions/{v}", idx((*Handler).duckdbVersion)},
		{"duckdb-versions/{v}/c-apis", map[string]rule{http.MethodPost: mb(serverAdmin, (*Handler).addCAPI)}},
		{"tenants", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).listTenants),
			http.MethodPost: mb(serverAdmin, (*Handler).createTenant)}},
		{"tenants/{t}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getTenant)}},
		{"tenants/{t}/suspend", map[string]rule{http.MethodPost: m(serverAdmin, (*Handler).suspend)}},
		{"tenants/{t}/resume", map[string]rule{http.MethodPost: m(serverAdmin, (*Handler).resume)}},
		{"tenants/{t}/whoami", idx((*Handler).whoami)},
		{"tenants/{t}/channels/{c}", idx((*Handler).channelInfo)},
		{"tenants/{t}/channels/{c}/extensions", idx((*Handler).extensions)},
		{"tenants/{t}/channels/{c}/extensions/{name}", idx((*Handler).extension)},
		{"tenants/{t}/channels/{c}/extensions/{name}/versions/{v}", idx((*Handler).item)},
		{"tenants/{t}/audiences", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listAudiences),
			http.MethodPost: mb(serverAdmin, (*Handler).addAudience)}},
		{"tenants/{t}/audiences/remove", map[string]rule{http.MethodPost: mb(serverAdmin, (*Handler).removeAudience)}},
		{"tenants/{t}/issuers", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listIssuers),
			http.MethodPost: mb(pathAdmin, (*Handler).addIssuer)}},
		{"tenants/{t}/issuers/{name}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getIssuer),
			http.MethodDelete: m(pathAdmin, (*Handler).removeIssuer)}},
		{"tenants/{t}/grants", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listGrants),
			http.MethodPost: mb(pathAdmin, (*Handler).addGrant)}},
		{"tenants/{t}/grants/{id}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getGrant),
			http.MethodDelete: m(pathAdmin, (*Handler).removeGrant)}},
		// phase 3: channels
		{"tenants/{t}/channels", map[string]rule{http.MethodGet: {access: public, handle: (*Handler).channels},
			http.MethodPost: mb(pathAdmin, (*Handler).createChannel)}},
		{"tenants/{t}/channels/{c}/duckdb-versions", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).channelVersions),
			http.MethodPost: mb(pathAdmin, (*Handler).addChannelVersion)}},
		{"tenants/{t}/channels/{c}/duckdb-versions/{v}", map[string]rule{http.MethodDelete: m(pathAdmin, (*Handler).removeChannelVersion)}},
		{"tenants/{t}/channels/{c}/keys", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).keyView),
			http.MethodPost: mb(serverAdmin, (*Handler).addKey)}},
		{"tenants/{t}/channels/{c}/keys/events", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).keyEvents)}},
		{"tenants/{t}/channels/{c}/keys/{id}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getKey)}},
		{"tenants/{t}/channels/{c}/keys/{id}/activate", map[string]rule{http.MethodPost: m(pathAdmin, (*Handler).activateKey)}},
		{"tenants/{t}/channels/{c}/keys/{id}/retire", map[string]rule{http.MethodPost: m(pathAdmin, (*Handler).retireKey)}},
		{"tenants/{t}/channels/{c}/releases", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).channelReleases)}},
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).extReleases)}},
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases/{id}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getRelease)}},
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases/{id}/{change}", map[string]rule{
			http.MethodPost: m(pathAdmin, (*Handler).changeRelease)}},
	}
}

// allowHeader lists a route's methods, HEAD with GET.
func (rt route) allowHeader() string {
	var ms []string
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete} {
		if _, ok := rt.methods[m]; ok || m == http.MethodHead && rt.methods[http.MethodGet].handle != nil {
			ms = append(ms, m)
		}
	}
	return strings.Join(ms, ", ")
}

// ServeHTTP implements http.Handler. kista serve calls it for /api/ on https requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow(client(r)) {
		w.Header().Set("Retry-After", h.limiter.retryAfter())
		problem(w, http.StatusTooManyRequests, typeTooMany, "")
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/api/v1/")
	if !ok || strings.ContainsAny(rest, "%\\") {
		notFound(w)
		return
	}
	segs := strings.Split(rest, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			notFound(w)
			return
		}
	}
	var rt route
	var p params
	found := false
	for _, x := range routes {
		if p, found = x.match(segs); found {
			rt = x
			break
		}
	}
	if !found {
		notFound(w)
		return
	}
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	ru, ok := rt.methods[method]
	if !ok {
		w.Header().Set("Allow", rt.allowHeader())
		problem(w, http.StatusMethodNotAllowed, typeMethod, "")
		return
	}
	c, ok := h.decide(w, r, rt, ru, p)
	if !ok {
		return
	}
	if !ru.body && (r.ContentLength != 0 || len(r.TransferEncoding) > 0) {
		problem(w, http.StatusBadRequest, typeInvalid, "this request has no body")
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	ru.handle(h, sw, r, c, p)
	if ru.manage && method != http.MethodGet {
		h.o.Log.Info("api: write", append(logIDs(rt, p), "actor", c.logName(), "method", r.Method,
			"status", sw.status, "location", sw.Header().Get("Location"), "client", client(r))...)
	}
}

// logIDs are a request's route template and the resource ids from its path.
func logIDs(rt route, p params) []any {
	out := []any{"route", rt.pattern}
	for _, k := range []string{"t", "c", "name", "ext", "v", "id", "change"} {
		if v, ok := p[k]; ok {
			out = append(out, k, v)
		}
	}
	return out
}

// logName is who the caller is in logs: its actor, or the server token it presented.
func (c caller) logName() string {
	if a, ok := c.actor(); ok {
		return a.String()
	}
	if c.server {
		return "server:" + c.id.Issuer.Name + "|" + c.id.Who() + " (not an administrator)"
	}
	return "anonymous"
}

// decide identifies the caller and decides from the path whether it may call the route, before any
// body is read or anything is looked up besides the tenant: 401 for a caller without a valid token,
// 404 for one that may not see or do it.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, rt route, ru rule, p params) (caller, bool) {
	ctx := r.Context()
	tok, sent := bearer(r)
	if sent && tok == "" {
		unauthorized(w)
		return caller{}, false
	}
	if !sent && ru.access != public {
		unauthorized(w) // without a token, anything not public is 401 before anything is looked up
		return caller{}, false
	}
	c, isServer, err := h.identifyServer(ctx, tok)
	if err != nil {
		unauthorized(w)
		return caller{}, false
	}
	name, onTenant := p["t"]
	switch {
	case onTenant:
		t, err := h.o.Store.GetTenant(ctx, name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.fail(w, err)
			return caller{}, false
		}
		// unknown and suspended tenants: 404 for everyone but server administrators
		if err != nil || t.State != store.TenantActive && !c.admin {
			notFound(w)
			return caller{}, false
		}
		c.tenant = t
		if sent && !isServer {
			if c, err = h.identifyTenant(ctx, t, tok); err != nil {
				if errors.Is(err, errToken) {
					unauthorized(w)
				} else {
					h.fail(w, err)
				}
				return caller{}, false
			}
		}
	case sent && !isServer && ru.access != public:
		// routes without a tenant take server tokens only
		if rt.pattern == "whoami" {
			problem(w, http.StatusBadRequest, typeInvalid, "not a server token: a tenant token asks /api/v1/tenants/{tenant}/whoami")
		} else {
			notFound(w)
		}
		return caller{}, false
	case !isServer:
		c = caller{} // a tenant token on a public route without a tenant is ignored
	}
	refuse := func() (caller, bool) {
		if !c.verified {
			unauthorized(w)
			return caller{}, false
		}
		h.o.Log.Info("api: refused", append(logIDs(rt, p), "actor", c.logName(), "method", r.Method, "status", http.StatusNotFound,
			"client", client(r))...)
		notFound(w)
		return caller{}, false
	}
	switch ru.access {
	case serverToken:
		if !c.server {
			return refuse()
		}
	case serverAdmin:
		if !c.admin {
			return refuse()
		}
	case pathAdmin:
		a, ok := c.actor()
		if !ok || h.o.Authz == nil {
			return refuse()
		}
		res := authz.Resource{Tenant: name, Channel: p["c"], Extension: p["ext"]}
		if err := h.o.Authz.Allow(ctx, a, authz.VerbAdmin, res); err != nil {
			if !errors.Is(err, authz.ErrDenied) {
				h.fail(w, err)
				return caller{}, false
			}
			return refuse()
		}
	}
	if ru.manage {
		if h.o.Tenants == nil || h.o.Auth == nil || h.o.Keys == nil || h.o.Releases == nil {
			notFound(w)
			return caller{}, false
		}
		// management needs a recently issued token (a node's long-lived install token is not
		// enough), and a write a named writer
		reason := ""
		switch {
		case c.id.IssuedAt.Before(h.o.Now().Add(-h.o.AdminTokenMaxAge)):
			reason = "stale token"
		case r.Method != http.MethodGet && r.Method != http.MethodHead && c.id.Who() == "":
			reason = "unnamed writer"
		}
		if reason != "" {
			h.o.Log.Info("api: refused", append(logIDs(rt, p), "actor", c.logName(), "method", r.Method,
				"status", http.StatusUnauthorized, "reason", reason, "client", client(r))...)
			unauthorized(w)
			return caller{}, false
		}
	}
	return c, true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	h.o.Log.Error("api: request failed", "error", err)
	problem(w, http.StatusServiceUnavailable, typeUnavailable, "")
}

// statusWriter records the status for the write log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (h *Handler) info(w http.ResponseWriter, r *http.Request, _ caller, _ params) {
	answer(w, r, map[string]string{"kista": h.o.KistaVersion, "api": Version}, true)
}
