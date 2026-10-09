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

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
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
	// Providers verify trusted-publishing tokens (spec 0008).
	Providers auth.Providers
	// Authz, Tenants and Auth are management's authorizer and services (with that authorizer); nil
	// Tenants or Auth: no management routes.
	Authz            authz.Authorizer
	Tenants          *tenants.Service
	Auth             *tenants.AuthAdmin
	Keys             *keys.Service     // phase 3
	Releases         *release.Service  // phase 3
	Upstreams        *upstream.Service // spec 0009
	Events           Recorder          // spec 0010: refusals and failures; nil: none recorded
	AdminTokenMaxAge time.Duration     // default 1h
	// Uploads (spec 0008): the largest file, the least read rate, and how many may run at once per
	// principal and per tenant (defaults 2 and 8).
	MaxBody, MinRate int64
	PublishPerActor  int
	PublishPerTenant int
	PublishMax       int // uploads at once on this server: fewer than the blob service's ingest slots
	Now              func() time.Time
}

// Handler serves /api/v1.
type Handler struct {
	o        Options
	limiter  *limiter
	failures *auth.FailureLog

	mu      sync.Mutex
	egress  map[string]bool // tenant id -> an issuer add (discovery, JWKS) is running
	uploads map[string]int  // actor and tenant id -> uploads running
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
	if o.PublishPerActor == 0 {
		o.PublishPerActor = 2
	}
	if o.PublishPerTenant == 0 {
		o.PublishPerTenant = 8
	}
	if o.PublishMax == 0 {
		o.PublishMax = 3
	}
	if o.MinRate == 0 {
		o.MinRate = 16 << 10
	}
	if o.MaxBody == 0 {
		o.MaxBody = 1 << 30
	}
	return &Handler{o: o, limiter: newLimiter(o.Rate, o.Burst), failures: &auth.FailureLog{Log: o.Log}, egress: map[string]bool{},
		uploads: map[string]int{}}
}

// WithClient records the request's client address (kista serve computes it, spec 0006; the
// request's facts are audit's, spec 0010).
func WithClient(ctx context.Context, addr string) context.Context {
	rq := audit.FromContext(ctx)
	rq.Client = addr
	return audit.WithRequest(ctx, rq)
}

func client(r *http.Request) string {
	if c := audit.FromContext(r.Context()).Client; c != "" {
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
	principals auth.Principals   // a tenant token's
	id         auth.Identity     // a verified token's identity
	verified   bool              // a valid token (server or tenant)
	server     bool              // a valid server token
	admin      bool              // a server administrator's
	pub        *auth.Publication // a publisher's credential (spec 0008)
}

func (c caller) anonymous() bool { return !c.verified }

// actor is the caller as the services see it; false for a caller with no rights to act on anything
// (anonymous, a server token that is not an administrator's).
func (c caller) actor() (authz.Actor, bool) {
	switch {
	case c.admin:
		return authz.Actor{Kind: authz.ActorServer, ID: c.id.Issuer.Name + "|" + c.id.Who()}, true
	case c.pub != nil:
		names := make([]string, 0, len(c.pub.Publishers))
		for _, m := range c.pub.Publishers {
			names = append(names, m.Publisher.Name)
		}
		return authz.Actor{Kind: authz.ActorPublisher, ID: c.tenant.Name + "/" + strings.Join(names, ","),
			Tenant: c.tenant.Name, Principals: c.principals}, true
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
	ta, canonical := auth.ForTenant(ta, h.o.PublicURL, t.Name, h.o.Providers)
	// a trusted-publishing provider's token is verified against the provider only (spec 0008)
	if p := h.o.Providers.For(auth.Issuer(tok)); p != nil {
		pub, err := p.Verify(ctx, ta, canonical, tok)
		if err != nil {
			h.failures.Record(t, "api", err)
			return c, errToken
		}
		c.principals, c.ta, c.id, c.verified, c.pub = pub.Identity.Principals, ta, pub.Identity, true, &pub
		return c, nil
	}
	id, err := h.o.Verifier.VerifyIdentity(ctx, ta, canonical, tok)
	if err != nil {
		h.failures.Record(t, "api", err)
		return c, errToken
	}
	c.principals, c.ta, c.id, c.verified = id.Principals, ta, id, true
	return c, nil
}

// identifyAPIKey finds a publisher's key by its hash: the key's tenant must be the path's, and it
// must not have expired (spec 0008).
func (h *Handler) identifyAPIKey(ctx context.Context, t store.Tenant, tok string) (caller, error) {
	c := caller{tenant: t}
	if !auth.WellFormedAPIKey(tok) {
		return c, errToken
	}
	k, p, err := h.o.Store.APIKeyByHash(ctx, auth.APIKeyHash(tok))
	if errors.Is(err, store.ErrNotFound) {
		return c, errToken
	}
	if err != nil {
		return c, err
	}
	now := h.o.Now()
	if p.TenantID != t.ID || !now.Before(k.ExpiresAt) {
		return c, errToken
	}
	if now.Sub(k.LastUsedAt) > time.Hour {
		if err := h.o.Store.TouchAPIKey(ctx, k.ID); err != nil {
			h.o.Log.Warn("api: recording a key's use", "error", err)
		}
	}
	ta, err := h.o.Auths.Get(ctx, t)
	if err != nil {
		return c, err
	}
	// a key is issued now for the freshness check (admin_token_max_age): its expiry and its removal
	// bound it, and it reaches publication and promotion only
	id := auth.Identity{Principals: auth.Principals{auth.PublisherKey(p.ID): true}, IssuedAt: now}
	c.principals, c.ta, c.id, c.verified = id.Principals, ta, id, true
	c.pub = &auth.Publication{Identity: id, Provider: "apikey",
		Publishers: []auth.MatchedPublisher{{Publisher: p, Credential: "key:" + k.ID}}}
	return c, nil
}

// --- routing ---

// access says who may call a route; decided from the path before anything else.
type access int

const (
	public          access = iota // anyone; a tenant route identifies a token for the view, others ignore one
	serverToken                   // a valid server token
	serverAdmin                   // a server administrator
	pathAdmin                     // admin on the path's resource (tenant, channel {c}, extension {ext}), or a server administrator
	pathVerbs                     // any of the rule's verbs on the path's resource (spec 0008: publish, promote), or a server administrator
	tenantPrincipal               // a token's principal of the path's tenant (its handler decides what it reads), or a server administrator
)

type handler func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params)

type route struct {
	pattern string // segments; {x} matches any one
	methods map[string]rule
}

// rule is one method of a route.
type rule struct {
	access access
	verbs  []authz.Verb // for pathVerbs
	manage bool         // management: a fresh token; for a write, a named writer
	body   bool         // the request has a JSON body; any other request has none
	raw    bool         // the request's body is a file (the publication route, spec 0008)
	// publishers marks the routes a publisher's credential may call (spec 0008); on other public
	// routes it is anonymous, on the rest not a token
	publishers bool
	handle     handler
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

// readers may read an extension's releases: its administrators and its publishers (spec 0008).
var readers = []authz.Verb{authz.VerbAdmin, authz.VerbPublish, authz.VerbPromote}

// auditors read a tenant's events (spec 0010): audit, which admin on the tenant implies.
var auditors = []authz.Verb{authz.VerbAudit}

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
		{"tenants/{t}/whoami", map[string]rule{http.MethodGet: {access: public, publishers: true, handle: (*Handler).whoami}}},
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
		// spec 0008: publication, promotion; the extension's release reads are open to publishers
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases", map[string]rule{
			http.MethodGet: {access: pathVerbs, verbs: readers, manage: true, publishers: true, handle: (*Handler).extReleases},
			http.MethodPost: {access: pathVerbs, verbs: []authz.Verb{authz.VerbPublish}, manage: true, raw: true, publishers: true,
				handle: (*Handler).publishRelease}}},
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases/promote", map[string]rule{
			http.MethodPost: {access: pathVerbs, verbs: []authz.Verb{authz.VerbPromote}, manage: true, body: true, publishers: true,
				handle: (*Handler).promoteRelease}}},
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases/{id}", map[string]rule{
			http.MethodGet: {access: pathVerbs, verbs: readers, manage: true, publishers: true, handle: (*Handler).getRelease}}},
		// spec 0010: events, read with audit on the tenant (admin implies it) or by server administrators
		{"tenants/{t}/events", map[string]rule{http.MethodGet: {access: pathVerbs, verbs: auditors, manage: true, handle: (*Handler).tenantEvents}}},
		{"tenants/{t}/events/{id}", map[string]rule{http.MethodGet: {access: pathVerbs, verbs: auditors, manage: true, handle: (*Handler).tenantEvent}}},
		{"events", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).serverEvents)}},
		// spec 0010 phase 2: download statistics
		{"tenants/{t}/stats/downloads", map[string]rule{http.MethodGet: m(tenantPrincipal, (*Handler).statsDownloads)}},
		{"tenants/{t}/stats/releases", map[string]rule{http.MethodGet: m(tenantPrincipal, (*Handler).statsReleases)}},
		{"events/{id}", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).serverEvent)}},
		{"event-kinds", idx((*Handler).eventKinds)},
		{"tenants/{t}/publishers", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listPublishers),
			http.MethodPost: mb(pathAdmin, (*Handler).addPublisher)}},
		{"tenants/{t}/publishers/{name}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getPublisher),
			http.MethodDelete: m(pathAdmin, (*Handler).removePublisher)}},
		{"tenants/{t}/publishers/{name}/github", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listGitHub),
			http.MethodPost: mb(pathAdmin, (*Handler).addGitHub)}},
		{"tenants/{t}/publishers/{name}/keys", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listAPIKeys),
			http.MethodPost: mb(pathAdmin, (*Handler).addAPIKey)}},
		{"tenants/{t}/publishers/{name}/keys/{id}", map[string]rule{http.MethodDelete: m(pathAdmin, (*Handler).removeAPIKey)}},
		{"tenants/{t}/publishers/{name}/github/{id}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getGitHub),
			http.MethodDelete: m(pathAdmin, (*Handler).removeGitHub)}},
		{"tenants/{t}/blocks", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listBlocks),
			http.MethodPost: mb(pathAdmin, (*Handler).addBlock)}},
		{"tenants/{t}/blocks/{hash}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getBlock),
			http.MethodDelete: m(pathAdmin, (*Handler).removeBlock)}},
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases/{id}/{change}", map[string]rule{
			http.MethodPost: m(pathAdmin, (*Handler).changeRelease)}},
		// spec 0009: upstreams (tenant administrators; {entry}, never {ext}: an extension's
		// administrators do not manage upstreams)
		{"tenants/{t}/upstreams", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listUpstreams),
			http.MethodPost: mb(pathAdmin, (*Handler).addUpstream)}},
		{"tenants/{t}/upstreams/{name}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getUpstream),
			http.MethodDelete: m(pathAdmin, (*Handler).removeUpstream)}},
		{"tenants/{t}/upstreams/{name}/public", map[string]rule{http.MethodPost: m(pathAdmin, upstreamAction(store.Public, ""))}},
		{"tenants/{t}/upstreams/{name}/private", map[string]rule{http.MethodPost: m(pathAdmin, upstreamAction(store.Private, ""))}},
		{"tenants/{t}/upstreams/{name}/pause", map[string]rule{http.MethodPost: m(pathAdmin, upstreamAction("", store.UpstreamPaused))}},
		{"tenants/{t}/upstreams/{name}/resume", map[string]rule{http.MethodPost: m(pathAdmin, upstreamAction("", store.UpstreamActive))}},
		{"tenants/{t}/upstreams/{name}/sync", map[string]rule{http.MethodPost: m(pathAdmin, (*Handler).syncUpstream)}},
		{"tenants/{t}/upstreams/{name}/cells", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listCells)}},
		{"tenants/{t}/upstreams/{name}/extensions", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listEntries),
			http.MethodPost: mb(pathAdmin, (*Handler).putEntry)}},
		{"tenants/{t}/upstreams/{name}/extensions/{entry}", map[string]rule{http.MethodDelete: m(pathAdmin, (*Handler).removeEntry)}},
		{"tenants/{t}/upstreams/{name}/platforms", map[string]rule{http.MethodGet: m(pathAdmin, listUpstreamItems("platforms")),
			http.MethodPost: mb(pathAdmin, addUpstreamItem("platforms"))}},
		{"tenants/{t}/upstreams/{name}/platforms/{item}", map[string]rule{http.MethodDelete: m(pathAdmin, removeUpstreamItem("platforms"))}},
		{"tenants/{t}/upstreams/{name}/keys", map[string]rule{http.MethodGet: m(pathAdmin, listUpstreamItems("keys")),
			http.MethodPost: mb(pathAdmin, addUpstreamItem("keys"))}},
		{"tenants/{t}/shadows", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listShadows)}},
		{"tenants/{t}/shadows/{name}", map[string]rule{http.MethodDelete: m(pathAdmin, (*Handler).removeShadow)}},
		{"tenants/{t}/upstreams/{name}/keys/{item}", map[string]rule{http.MethodDelete: m(pathAdmin, removeUpstreamItem("keys"))}},
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
	sw := &statusWriter{ResponseWriter: w}
	defer h.record(r, rt, p, method, sw) // a refusal or a failure is an event (spec 0010)
	c, ok := h.decide(sw, r, rt, ru, p)
	if !ok {
		return // a refusal noted its actor
	}
	sw.actor = c
	if name := audit.DisplayName(c.id.Claims); name != "" { // for the events the request causes (spec 0010)
		r = r.WithContext(audit.WithActorName(r.Context(), name))
	}
	if !ru.body && !ru.raw && (r.ContentLength != 0 || len(r.TransferEncoding) > 0) {
		problem(sw, http.StatusBadRequest, typeInvalid, "this request has no body")
		return
	}
	ru.handle(h, sw, r, c, p)
	if ru.manage && method != http.MethodGet {
		h.o.Log.Info("api: write", append(logIDs(rt, p), "actor", c.logName(), "method", r.Method,
			"status", sw.status, "location", sw.Header().Get("Location"), "client", client(r))...)
	}
}

// logIDs are a request's route template and the resource ids from its path.
func logIDs(rt route, p params) []any {
	out := []any{"route", rt.pattern}
	for _, k := range []string{"t", "c", "name", "ext", "v", "id", "change", "hash"} {
		if v, ok := p[k]; ok {
			out = append(out, k, v)
		}
	}
	return out
}

// eventActor is the caller as events name it (spec 0010): its actor, server:<issuer>|<who> for a
// server token that is no administrator's, or anonymous.
func (c caller) eventActor() string {
	if a, ok := c.actor(); ok {
		return a.String()
	}
	if c.server {
		return "server:" + c.id.Issuer.Name + "|" + c.id.Who()
	}
	return "anonymous"
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
	failed := func(reason string) { note(w, func(sw *statusWriter) { sw.authFailure = reason }) }
	if sent && tok == "" {
		failed("malformed")
		unauthorized(w)
		return caller{}, false
	}
	if !sent && ru.access != public {
		unauthorized(w) // without a token, anything not public is 401 before anything is looked up
		return caller{}, false
	}
	// an API key (spec 0008) is looked up only on the routes publishers use
	if auth.IsAPIKey(tok) && (!ru.publishers || p["t"] == "") {
		failed("api_key_route")
		unauthorized(w)
		return caller{}, false
	}
	c, isServer, err := h.identifyServer(ctx, tok)
	if err != nil {
		failed("invalid")
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
		note(w, func(sw *statusWriter) { sw.tenant = t })
		if sent && !isServer {
			identify := h.identifyTenant
			if auth.IsAPIKey(tok) {
				identify = h.identifyAPIKey
			}
			if c, err = identify(ctx, t, tok); err != nil {
				if errors.Is(err, errToken) {
					failed("invalid")
					unauthorized(w)
				} else {
					h.fail(w, err)
				}
				return caller{}, false
			}
			if c.pub != nil && !ru.publishers {
				if ru.access != public {
					failed("publisher_route")
					unauthorized(w) // a publisher's credential is not a token for this route
					return caller{}, false
				}
				c = caller{tenant: t} // the public view
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
		note(w, func(sw *statusWriter) { sw.refused, sw.actor = true, c })
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
	case tenantPrincipal:
		if !c.admin && (c.principals == nil || c.pub != nil) {
			return refuse()
		}
	case pathVerbs:
		a, ok := c.actor()
		if !ok || h.o.Authz == nil {
			return refuse()
		}
		res := authz.Resource{Tenant: name, Channel: p["c"], Extension: p["ext"], Reserved: reserved.Kind(p["ext"]) != ""}
		allowed := false
		for _, v := range ru.verbs {
			err := h.o.Authz.Allow(ctx, a, v, res)
			if err == nil {
				allowed = true
				break
			}
			if !errors.Is(err, authz.ErrDenied) {
				h.fail(w, err)
				return caller{}, false
			}
		}
		if !allowed {
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
		case r.Method != http.MethodGet && r.Method != http.MethodHead && c.pub == nil && c.id.Who() == "":
			// (a publisher is named by its publishers, whatever its token's sub)
			reason = "unnamed writer"
		}
		if reason != "" {
			h.o.Log.Info("api: refused", append(logIDs(rt, p), "actor", c.logName(), "method", r.Method,
				"status", http.StatusUnauthorized, "reason", reason, "client", client(r))...)
			failed(strings.ReplaceAll(reason, " ", "_"))
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

// statusWriter records the status for the write log, and what an answer means for the events
// (spec 0010).
type statusWriter struct {
	http.ResponseWriter
	status int
	tenant store.Tenant // the path's tenant, once known
	actor  caller
	// authFailure is why a sent token was not accepted (a 401); refused, that a known caller was
	// refused (a 404 that stands for forbidden).
	authFailure string
	refused     bool
}

// note marks a statusWriter (w is one when the request came through ServeHTTP).
func note(w http.ResponseWriter, f func(*statusWriter)) {
	if sw, ok := w.(*statusWriter); ok {
		f(sw)
	}
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

// Recorder takes the events of refusals and failures (spec 0010's asynchronous writer).
type Recorder interface {
	Refusal(ctx context.Context, tenantID, actor string, kind audit.Kind, outcome, subject string, fields map[string]any)
}

// record writes a request's refusal or failure as an event, once, from its final answer: a sent
// token not accepted (401), a known caller refused (404 for forbidden), a 5xx.
func (h *Handler) record(r *http.Request, rt route, p params, method string, sw *statusWriter) {
	if h.o.Events == nil {
		return
	}
	route := method + " " + rt.pattern
	subject := "route:" + rt.pattern
	actor := sw.actor.eventActor()
	if sw.tenant.ID == "" && p["t"] != "" && (sw.authFailure != "" || sw.refused || sw.status >= 500) {
		// a failure noted before the tenant was looked up is still the tenant's, if it is one
		t, err := h.o.Store.GetTenant(context.WithoutCancel(r.Context()), p["t"]) // the client may be gone
		if err != nil || t.State != store.TenantActive {
			return // an unknown or suspended tenant: nobody's
		}
		sw.tenant = t
	}
	switch {
	case sw.authFailure != "":
		h.o.Events.Refusal(r.Context(), sw.tenant.ID, "anonymous", "auth.failure", audit.Refused, subject,
			map[string]any{"reason": sw.authFailure, "route": route})
	case sw.refused:
		h.o.Events.Refusal(r.Context(), sw.tenant.ID, actor, "authz.refused", audit.Refused, subject,
			map[string]any{"route": route, "status": sw.status})
	case sw.status >= 500:
		h.o.Events.Refusal(r.Context(), sw.tenant.ID, actor, "request.failed", audit.Failed, subject,
			map[string]any{"route": route, "status": sw.status})
	}
}
