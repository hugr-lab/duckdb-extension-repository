// Package api is /api/v1 (spec 0007): the index of what a channel serves to the caller, and
// management over the services kista admin uses. kista serve hands /api/ to it on https requests
// only.
package api

import (
	"bytes"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
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
	// Authz, Tenants and Auth are management's authorizer and services (with that authorizer); nil
	// Tenants or Auth: no management routes.
	Authz            authz.Authorizer
	Tenants          *tenants.Service
	Auth             *tenants.AuthAdmin
	Keys             *keys.Service       // phase 3
	Releases         *release.Service    // phase 3
	Upstreams        *upstream.Service   // spec 0009
	Attachments      *attachment.Service // spec 0011; nil: no attachment routes
	Feeds            *attachment.Feeds   // spec 0011 phase 1b; nil: no feed management routes
	Licenses         *licenses.Service   // spec 0012; nil: no licence routes
	Events           Recorder            // spec 0010: refusals and failures; nil: none recorded
	AdminTokenMaxAge time.Duration       // default 1h
	// MinSuspended is how long a tenant stays suspended before it may be deleted (spec 0019): 0 is
	// the default, 7 days; a negative value is none (tenants.min_suspended: 0 in the dev profile).
	MinSuspended time.Duration
	// Uploads (spec 0008): the largest file, the least read rate, and how many may run at once per
	// principal and per tenant (defaults 2 and 8).
	MaxBody, MinRate int64
	PublishPerActor  int
	PublishPerTenant int
	PublishMax       int // uploads at once on this server: fewer than the blob service's ingest slots
	Now              func() time.Time
	// Console is the administration console's sign-in configuration (spec 0015); nil: its routes
	// answer 404 (ui.enabled false).
	Console *Console
	// DistributorMinTenants and PublicTenants are statistics.distributor_min_tenants (below 2: the
	// default, 3) and statistics.public_tenants (spec 0036 phase 2: distributor statistics).
	DistributorMinTenants int
	PublicTenants         []string
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
	switch {
	case o.MinSuspended == 0:
		o.MinSuspended = 7 * 24 * time.Hour
	case o.MinSuspended < 0:
		o.MinSuspended = 0
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
	if o.DistributorMinTenants < 2 { // fail closed: never fewer than 2
		o.DistributorMinTenants = 3
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
	typeUnavailable  = "unavailable"
	// the 429s (spec 0037: one type per cause, so a client acts on the type)
	typeRateLimited   = "rate-limited"   // the per-client rate limiter
	typeUploadSlots   = "upload-slots"   // the upload slots per actor, tenant or server
	typeIngestBusy    = "ingest-busy"    // every ingest slot taken
	typeChunksBusy    = "chunks-busy"    // too many chunks of one bundle upload in flight
	typeCatalogueFull = "catalogue-full" // a source's catalogue full: no Retry-After, it clears as its builds go
	typeIssuerBusy    = "issuer-busy"    // an issuer being added to the tenant
	// typeSource: another source holds the name in the channel (spec 0031)
	typeSource = "source"
	// typeHeld: a held entry, or a publisher with a hold on a channel's route (spec 0033)
	typeHeld = "held"
	// typeWithdrawn: a withdrawn entry (spec 0033 phase B): placed nowhere, its release never made
	// active again, its body never arriving again in its source
	typeWithdrawn = "withdrawn"
	// typeDistributorsEntry: a write on a distributor's publisher's licence entry by a tenant
	// administrator (spec 0037), the one 404 with a type and a detail of its own: the caller reads
	// the licence and sees whose entry it is, so they reveal nothing
	typeDistributorsEntry = "distributors-entry"
	// typeStale: a signed metadata write older than the entry's last one, or of its second with another
	// body (spec 0036): the client reads again
	typeStale = "stale"
)

var titles = map[string]string{typeUnauthorized: "Unauthorized", typeNotFound: "Not Found", typeMethod: "Method Not Allowed",
	typeInvalid: "Invalid", typeConflict: "Conflict", typePrecondition: "Precondition Failed",
	typeRequired: "Precondition Required", typeTooLarge: "Content Too Large", typeMediaType: "Unsupported Media Type",
	typeRateLimited: "Rate Limited", typeUploadSlots: "Upload Slots Taken", typeIngestBusy: "Ingest Busy", typeChunksBusy: "Chunks Busy",
	typeCatalogueFull: "Catalogue Full", typeIssuerBusy: "Issuer Busy", typeUnavailable: "Service Unavailable", typeSource: "Another Source Holds the Name",
	typeHeld: "Held", typeWithdrawn: "Withdrawn", typeDistributorsEntry: "A Distributor's Entry", typeStale: "Stale"}

// problem writes application/problem+json. 401 and 404 have constant bytes.
func problem(w http.ResponseWriter, status int, typ, detail string) {
	writeProblem(w, status, typ, detail, status != http.StatusUnauthorized && status != http.StatusNotFound)
}

// writeProblem writes a problem; its detail only when shown is set (a 401 and a 404 carry none,
// except a signed request's 401, which names the key and the clock: spec 0023).
func writeProblem(w http.ResponseWriter, status int, typ, detail string, shown bool) {
	body := map[string]any{"type": "urn:kista:problem:" + typ, "title": titles[typ], "status": status}
	if detail != "" && shown {
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
	b = withWarnings(r, b)
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

// warningJSON is a licence warning in an act's answer (spec 0035 phase 2).
type warningJSON struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Channel string `json:"channel"`
	Message string `json:"message"`
}

type answerWarnings struct{}

// seesWarnings reports whether a write's answer carries its licence warnings: the caller
// administers the path (a server administrator; a tenant, channel or extension administrator),
// and is no publisher, licence manager or distribution admin.
func (h *Handler) seesWarnings(r *http.Request, c caller, p params) bool {
	switch {
	case c.admin:
		return true
	case c.pub != nil, c.licenseManager, c.distribution, c.principals == nil:
		return false
	}
	return h.isAdmin(r, c, p)
}

// withWarnings adds the request's licence warnings to an answer object as its "warnings" member
// (spec 0035 phase 2: one generic array, omitted when empty), after its ETag was taken, for a caller
// who sees them. An answer that has a top-level "warnings" member of its own keeps it as it is.
func withWarnings(r *http.Request, b []byte) []byte {
	ws := store.LicenseWarnings(r.Context())
	if len(ws) == 0 || len(b) < 2 || b[0] != '{' {
		return b
	}
	if sees, _ := r.Context().Value(answerWarnings{}).(func() bool); sees == nil || !sees() {
		return b
	}
	if bytes.Contains(b, []byte(`"warnings"`)) {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(b, &top); err != nil {
			return b
		}
		if _, ok := top["warnings"]; ok {
			return b
		}
	}
	out := make([]warningJSON, len(ws))
	for i, x := range ws {
		out[i] = warningJSON{Name: x.Name, Source: x.Source, Channel: x.Channel, Message: x.Message}
	}
	j, err := json.Marshal(out)
	if err != nil {
		return b
	}
	sep := ","
	if len(b) == 2 { // {}
		sep = ""
	}
	return slices.Concat(b[:len(b)-1], []byte(sep+`"warnings":`), j, []byte("}"))
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

// caller is who asks: anonymous, a server token (an administrator's, a licence manager's, or
// neither), or a tenant token's principals.
type caller struct {
	tenant         store.Tenant // on tenant routes
	ta             store.TenantAuth
	principals     auth.Principals     // a tenant token's
	id             auth.Identity       // a verified token's identity
	verified       bool                // a valid token (server or tenant)
	server         bool                // a valid server token
	admin          bool                // a server administrator's
	licenseManager bool                // a licence manager's (spec 0029); with admin, it acts as an administrator
	distribution   bool                // a distribution admin's (spec 0030); with admin, it acts as an administrator
	pub            *auth.Publication   // a publisher's credential (spec 0008)
	signed         *auth.SignedRequest // a publisher's signed request (spec 0023)
}

func (c caller) anonymous() bool { return !c.verified }

// held is what the caller's principals hold in the tenant (none for a caller without principals).
func (c caller) held() auth.Held { return auth.HeldBy(c.principals, c.ta) }

// actor is the caller as the services see it; false for a caller with no rights to act on anything
// (anonymous, a server token that is neither an administrator's nor a licence manager's).
func (c caller) actor() (authz.Actor, bool) {
	switch {
	case c.admin:
		return authz.Actor{Kind: authz.ActorServer, ID: c.id.Issuer.Name + "|" + c.id.Who()}, true
	case c.licenseManager:
		// spec 0029: the services allow it license on a tenant, and nothing else. A server token is
		// never both a licence manager's and a distribution admin's (auth.Server.Verify refuses one
		// whose principals hold both, 401); were it so, it would act as the licence manager, and the
		// distribution admins' routes would answer it 404 (spec 0037)
		return authz.Actor{Kind: authz.ActorLicenseManager, ID: c.id.Issuer.Name + "|" + c.id.Who()}, true
	case c.distribution:
		// spec 0030: the services allow it a distributor's calls, and nothing else
		return authz.Actor{Kind: authz.ActorDistributionAdmin, ID: c.id.Issuer.Name + "|" + c.id.Who()}, true
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
	id, roles, err := h.o.Server.Verify(ctx, tok)
	if err != nil {
		h.failures.Record(store.Tenant{Name: "(server)"}, "api", err)
		return caller{}, true, errToken
	}
	return caller{id: id, verified: true, server: true, admin: roles.Admin, licenseManager: roles.LicenseManager,
		distribution: roles.DistributionAdmin}, true, nil
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
	public            access = iota // anyone; a tenant route identifies a token for the view, others ignore one
	serverToken                     // a valid server token
	serverAdmin                     // a server administrator
	pathAdmin                       // admin on the path's resource (tenant, channel {c}, extension {ext}, extension group {g} to read it), or a server administrator
	pathVerbs                       // any of the rule's verbs on the path's resource (spec 0008: publish, promote), or a server administrator
	tenantPrincipal                 // a token's principal of the path's tenant (its handler decides what it reads), or a server administrator
	licenseAdmin                    // pathAdmin, or a licence manager (spec 0029): the tenant's licences only
	distributionAdmin               // pathAdmin, or a distribution admin (spec 0030): its service narrows each call
	// tenantCaller: any verified caller of the path's tenant (spec 0036: an entry's metadata), a
	// signed request from any of its publishers included on a route that admits publishers; its
	// service decides, every refusal the same 404
	tenantCaller
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
	// distributors admits a distributor's publisher on a pathVerbs route without the verbs (spec 0033
	// phase B: the catalogue-only publication, whose service checks the name), and on a
	// distributionAdmin route with publishers (spec 0037: its own catalogue entries, which its service
	// narrows it to)
	distributors bool
	// closed: server administrators reach a tenant whose deletion started (spec 0019: its record,
	// its deletion and its events); to everyone else it is unknown
	closed bool
	// suspended: a distribution admin reaches a suspended tenant (spec 0037: its state, so a
	// distributor tells "suspended, wait" from "gone"); to everyone else but server administrators it
	// is unknown
	suspended bool
	handle    handler
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

// attachmentReaders may reach the extension's attachment routes (spec 0011; the service decides
// exactly), attachmentWriters may attach (publishers attach an SBOM or provenance).
var (
	attachmentReaders = []authz.Verb{authz.VerbAdmin, authz.VerbAudit}
	attachmentWriters = []authz.Verb{authz.VerbAdmin, authz.VerbPublish}
)

// auditors read a tenant's events (spec 0010): audit, which admin on the tenant implies.
var auditors = []authz.Verb{authz.VerbAudit}

func init() {
	idx := func(f handler) map[string]rule { return map[string]rule{http.MethodGet: {access: public, handle: f}} }
	m := func(a access, f handler) rule { return rule{access: a, manage: true, handle: f} }
	mb := func(a access, f handler) rule { return rule{access: a, manage: true, body: true, handle: f} }
	routes = []route{
		{"info", idx((*Handler).info)},
		// spec 0015: the administration console's sign-in configuration
		{"console", map[string]rule{http.MethodGet: {access: public, handle: (*Handler).serverConsole}}},
		{"tenants/{t}/console", map[string]rule{http.MethodGet: {access: public, handle: (*Handler).tenantConsole}}},
		{"whoami", map[string]rule{http.MethodGet: {access: serverToken, handle: (*Handler).serverWhoami}}},
		{"duckdb-versions", map[string]rule{http.MethodGet: {access: public, handle: (*Handler).duckdbVersions},
			http.MethodPost: mb(serverAdmin, (*Handler).addDuckDBVersion)}},
		{"duckdb-versions/{v}", idx((*Handler).duckdbVersion)},
		{"duckdb-versions/{v}/c-apis", map[string]rule{http.MethodPost: mb(serverAdmin, (*Handler).addCAPI)}},
		{"duckdb-versions/{v}/core-extensions", map[string]rule{http.MethodPost: mb(serverAdmin, (*Handler).addCoreExtensions)}},
		// spec 0030: the server's reserved extension names
		{"reserved-names", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).listReserved),
			http.MethodPost: mb(serverAdmin, (*Handler).addReserved)}},
		{"reserved-names/{name}", map[string]rule{http.MethodDelete: m(serverAdmin, (*Handler).removeReserved)}},
		// spec 0030: names reserved in a tenant (server administrators and distribution admins write;
		// the tenant's administrators read)
		{"tenants/{t}/reserved-names", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).listTenantReserved)}},
		{"tenants/{t}/reserved-names/{name}", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).getTenantReserved),
			http.MethodPut:    mb(distributionAdmin, (*Handler).reserveInTenant),
			http.MethodDelete: m(distributionAdmin, (*Handler).releaseInTenant)}},
		{"tenants", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).listTenants),
			http.MethodPost: mb(serverAdmin, (*Handler).createTenant)}},
		// spec 0037: a distribution admin reads a tenant's state and expected holder
		{"tenants/{t}", map[string]rule{http.MethodGet: {access: distributionAdmin, manage: true, closed: true, suspended: true,
			handle: (*Handler).getTenant},
			http.MethodDelete: {access: serverAdmin, manage: true, closed: true, handle: (*Handler).deleteTenant}}},
		{"tenants/{t}/suspend", map[string]rule{http.MethodPost: m(serverAdmin, (*Handler).suspend)}},
		// spec 0028: the holder the tenant's licences must name
		{"tenants/{t}/holder", map[string]rule{http.MethodPut: mb(serverAdmin, (*Handler).setTenantHolder),
			http.MethodDelete: m(serverAdmin, (*Handler).clearTenantHolder)}},
		// bundles uploaded over the API (spec 0014 phase 2), and every import's record
		{"tenants/{t}/bundle-uploads", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).listUploads),
			http.MethodPost: mb(serverAdmin, (*Handler).createUpload)}},
		{"tenants/{t}/bundle-uploads/{id}", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).getUpload),
			http.MethodDelete: m(serverAdmin, (*Handler).abortUpload)}},
		{"tenants/{t}/bundle-uploads/{id}/complete", map[string]rule{http.MethodPost: m(serverAdmin, (*Handler).completeUpload)}},
		{"tenants/{t}/bundle-uploads/{id}/chunks/{n}", map[string]rule{http.MethodPut: {access: serverAdmin, manage: true, raw: true,
			handle: (*Handler).putChunk}}},
		{"tenants/{t}/bundle-imports", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).listImports)}},
		{"tenants/{t}/bundle-imports/{id}", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).getImport)}},
		{"tenants/{t}/resume", map[string]rule{http.MethodPost: m(serverAdmin, (*Handler).resume)}},
		{"tenants/{t}/whoami", map[string]rule{http.MethodGet: {access: public, publishers: true, handle: (*Handler).whoami}}},
		{"tenants/{t}/channels/{c}", idx((*Handler).channelInfo)},
		{"tenants/{t}/channels/{c}/extensions", idx((*Handler).extensions)},
		{"tenants/{t}/channels/{c}/extensions/{name}", idx((*Handler).extension)},
		{"tenants/{t}/channels/{c}/extensions/{name}/versions/{v}", idx((*Handler).item)},
		// spec 0011: attachments, read by readers through the index, managed under the extension
		{"tenants/{t}/channels/{c}/extensions/{name}/attachments", idx((*Handler).indexAttachments)},
		{"tenants/{t}/channels/{c}/extensions/{name}/attachments/{id}", idx((*Handler).indexAttachment)},
		{"tenants/{t}/extensions/{ext}/attachments", map[string]rule{
			http.MethodGet: {access: pathVerbs, verbs: attachmentReaders, manage: true, handle: attachmentRoute((*Handler).listAttachments)},
			http.MethodPost: {access: pathVerbs, verbs: attachmentWriters, manage: true, raw: true, publishers: true,
				handle: attachmentRoute((*Handler).addAttachment)}}},
		{"tenants/{t}/extensions/{ext}/attachments/{id}", map[string]rule{
			http.MethodGet:    {access: pathVerbs, verbs: attachmentReaders, manage: true, handle: attachmentRoute((*Handler).getAttachment)},
			http.MethodDelete: {access: pathVerbs, verbs: attachmentReaders, manage: true, handle: attachmentRoute((*Handler).removeAttachment)}}},
		{"tenants/{t}/extensions/{ext}/attachments/{id}/content", map[string]rule{
			http.MethodGet: {access: pathVerbs, verbs: attachmentReaders, manage: true, handle: attachmentRoute((*Handler).attachmentContent)}}},
		{"tenants/{t}/extensions/{ext}/attachments/{id}/{change}", map[string]rule{
			http.MethodPost: m(pathAdmin, attachmentRoute((*Handler).changeAttachment))}},
		{"tenants/{t}/blocks/{hash}/attachments", map[string]rule{http.MethodGet: m(pathAdmin, attachmentRoute((*Handler).blockAttachments))}},
		{"tenants/{t}/blocks/{hash}/attachments/{id}", map[string]rule{http.MethodDelete: m(pathAdmin, attachmentRoute((*Handler).removeBlockAttachment))}},
		// spec 0011 phase 1b: attestation keys (public to read), the feed a tenant serves (to
		// principals holding feed; not management: a feed credential may be long-lived), its feeds
		{"tenants/{t}/attestation-keys", map[string]rule{http.MethodGet: {access: public, handle: (*Handler).attestationKeys},
			http.MethodPost: mb(serverAdmin, (*Handler).addAttestationKey)}},
		{"tenants/{t}/attestation-keys/{id}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getAttestationKey)}},
		{"tenants/{t}/attestation-keys/{id}/activate", map[string]rule{http.MethodPost: m(pathAdmin, attestationKeyState(true))}},
		{"tenants/{t}/attestation-keys/{id}/retire", map[string]rule{http.MethodPost: m(pathAdmin, attestationKeyState(false))}},
		{"tenants/{t}/feed", map[string]rule{http.MethodPost: {access: tenantPrincipal, body: true, handle: attachmentRoute((*Handler).feed)}}},
		{"tenants/{t}/feed/documents/{id}", map[string]rule{http.MethodGet: {access: tenantPrincipal, handle: attachmentRoute((*Handler).feedDocument)}}},
		{"tenants/{t}/feeds", map[string]rule{http.MethodGet: m(pathAdmin, feedRoute((*Handler).listFeeds)),
			http.MethodPost: mb(pathAdmin, feedRoute((*Handler).addFeed))}},
		{"tenants/{t}/feeds/{name}", map[string]rule{http.MethodGet: m(pathAdmin, feedRoute((*Handler).getFeed)),
			http.MethodPost: mb(pathAdmin, feedRoute((*Handler).changeFeed)), http.MethodDelete: m(pathAdmin, feedRoute((*Handler).removeFeed))}},
		{"tenants/{t}/feeds/{name}/sync", map[string]rule{http.MethodPost: m(pathAdmin, feedRoute((*Handler).syncFeed))}},
		{"tenants/{t}/audiences", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listAudiences),
			http.MethodPost: mb(serverAdmin, (*Handler).addAudience)}},
		{"tenants/{t}/audiences/remove", map[string]rule{http.MethodPost: mb(serverAdmin, (*Handler).removeAudience)}},
		{"tenants/{t}/issuers", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).listIssuers),
			http.MethodPost: mb(pathAdmin, (*Handler).addIssuer)}},
		{"tenants/{t}/issuers/{name}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getIssuer),
			http.MethodDelete: m(pathAdmin, (*Handler).removeIssuer)}},
		{"tenants/{t}/issuers/{name}/console", map[string]rule{http.MethodPost: mb(pathAdmin, (*Handler).setConsoleClient),
			http.MethodDelete: m(pathAdmin, (*Handler).removeConsoleClient)}},
		{"tenants/{t}/grants", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).listGrants),
			http.MethodPost: mb(distributionAdmin, (*Handler).addGrant)}},
		{"tenants/{t}/grants/principals", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).grantPrincipals)}}, // before grants/{id}
		{"tenants/{t}/grants/{id}", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).getGrant),
			http.MethodDelete: m(distributionAdmin, (*Handler).removeGrant)}},
		// spec 0017: extension groups; the services decide who besides a tenant administrator reads
		{"tenants/{t}/extension-groups", map[string]rule{http.MethodGet: m(tenantPrincipal, (*Handler).listGroups),
			http.MethodPost: mb(pathAdmin, (*Handler).createGroup)}},
		{"tenants/{t}/extension-groups/{g}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).getGroup),
			http.MethodDelete: m(pathAdmin, (*Handler).removeGroup)}},
		{"tenants/{t}/extension-groups/{g}/description", map[string]rule{http.MethodPost: mb(pathAdmin, (*Handler).setGroupDescription)}},
		{"tenants/{t}/extension-groups/{g}/members", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).groupMembers),
			http.MethodPost: mb(pathAdmin, (*Handler).changeGroupMembers)}},
		// phase 3: channels
		{"tenants/{t}/channels", map[string]rule{http.MethodGet: {access: public, handle: (*Handler).channels},
			http.MethodPost: mb(pathAdmin, (*Handler).createChannel)}},
		{"tenants/{t}/channels/{c}/duckdb-versions", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).channelVersions),
			http.MethodPost: mb(pathAdmin, (*Handler).addChannelVersion)}},
		{"tenants/{t}/channels/{c}/duckdb-versions/{v}", map[string]rule{http.MethodDelete: m(pathAdmin, (*Handler).removeChannelVersion)}},
		// spec 0033: the tenant's catalogue, read by its administrators
		// (spec 0033 phase B: and distribution admins, their distributors' publishers' entries only)
		// (spec 0037: and a distributor's publisher, signing the request, its own entries)
		{"tenants/{t}/catalogue", map[string]rule{http.MethodGet: {access: distributionAdmin, manage: true, publishers: true, distributors: true,
			handle: (*Handler).catalogue}}},
		// publishing into the catalogue only: publish on the name tenant-wide (a publisher's signed
		// request), or a distributor's publisher without grants (spec 0033 phase B: its service checks
		// the name); before the entry's route, which an extension named "extensions" would match
		{"tenants/{t}/catalogue/extensions/{ext}/builds", map[string]rule{
			http.MethodPost: {access: pathVerbs, verbs: []authz.Verb{authz.VerbPublish}, manage: true, raw: true, publishers: true,
				distributors: true, handle: (*Handler).publishToCatalogue}}},
		{"tenants/{t}/catalogue/{source}/extensions/{ext}", map[string]rule{http.MethodGet: {access: distributionAdmin, manage: true,
			publishers: true, distributors: true, handle: (*Handler).catalogueEntry}}},
		{"tenants/{t}/catalogue/builds/{id}", map[string]rule{http.MethodDelete: m(distributionAdmin, (*Handler).purgeEntry)}},
		// spec 0036: an entry's metadata and its icon, an access rule of their own: any verified caller
		// of the tenant, any of its publishers' signed requests included; the service decides by the
		// entry's source
		{"tenants/{t}/catalogue/{source}/extensions/{ext}/meta", map[string]rule{
			http.MethodGet: {access: tenantCaller, manage: true, publishers: true, handle: (*Handler).entryMeta},
			http.MethodPut: {access: tenantCaller, manage: true, body: true, publishers: true, handle: (*Handler).setEntryMeta}}},
		{"tenants/{t}/catalogue/{source}/extensions/{ext}/icon", map[string]rule{
			http.MethodGet: {access: tenantCaller, manage: true, publishers: true, handle: (*Handler).entryIcon}}},
		// spec 0033 phase B: withdrawing a version of a distributor's publisher (distribution admins and
		// server administrators; the service refuses a tenant's administrators)
		{"tenants/{t}/catalogue/{source}/extensions/{ext}/versions/{v}/withdraw", map[string]rule{
			http.MethodPost: mb(distributionAdmin, (*Handler).withdraw)}},
		// spec 0031: a name's source in a channel ({name}, not {ext}: a channel's administrators)
		{"tenants/{t}/channels/{c}/sources", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).channelSources)}},
		// spec 0033: a channel following a whole source (its administrators')
		{"tenants/{t}/channels/{c}/follows", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).channelFollows)}},
		{"tenants/{t}/channels/{c}/follows/{source}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).channelFollow),
			http.MethodPut: m(pathAdmin, (*Handler).setChannelFollow), http.MethodDelete: m(pathAdmin, (*Handler).removeChannelFollow)}},
		{"tenants/{t}/channels/{c}/sources/{name}", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).channelSource),
			http.MethodPut: mb(pathAdmin, (*Handler).setChannelSource), http.MethodDelete: m(pathAdmin, (*Handler).clearChannelSource)}},
		// spec 0033 phase A2: a channel's hold (its administrators')
		{"tenants/{t}/channels/{c}/hold", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).channelHold),
			http.MethodPut: mb(pathAdmin, (*Handler).setChannelHold)}},
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
		// spec 0033: placing a catalogue build by hand (promote on the name, as a promotion)
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases/place", map[string]rule{
			http.MethodPost: {access: pathVerbs, verbs: []authz.Verb{authz.VerbPromote}, manage: true, body: true, publishers: true,
				handle: (*Handler).placeRelease}}},
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases/promote", map[string]rule{
			http.MethodPost: {access: pathVerbs, verbs: []authz.Verb{authz.VerbPromote}, manage: true, body: true, publishers: true,
				handle: (*Handler).promoteRelease}}},
		{"tenants/{t}/channels/{c}/extensions/{ext}/releases/{id}", map[string]rule{
			http.MethodGet: {access: pathVerbs, verbs: readers, manage: true, publishers: true, handle: (*Handler).getRelease},
			// spec 0016 phase 2: purging a yanked release
			http.MethodDelete: m(pathAdmin, (*Handler).purgeRelease)}},
		// spec 0010: events, read with audit on the tenant (admin implies it) or by server administrators
		{"tenants/{t}/events", map[string]rule{http.MethodGet: {access: pathVerbs, verbs: auditors, manage: true, closed: true, handle: (*Handler).tenantEvents}}},
		{"tenants/{t}/events/{id}", map[string]rule{http.MethodGet: {access: pathVerbs, verbs: auditors, manage: true, closed: true,
			handle: (*Handler).tenantEvent}}},
		{"events", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).serverEvents)}},
		// spec 0010 phase 2a: download statistics
		{"tenants/{t}/stats/downloads", map[string]rule{http.MethodGet: m(tenantPrincipal, (*Handler).statsDownloads)}},
		{"tenants/{t}/stats/releases", map[string]rule{http.MethodGet: m(tenantPrincipal, (*Handler).statsReleases)}},
		{"tenants/{t}/stats/usage", map[string]rule{http.MethodGet: m(tenantPrincipal, (*Handler).statsUsage)}},
		// spec 0036 phase 2: a distributor's downloads summed over the tenants (distribution admins
		// and server administrators)
		{"distributor-stats/downloads", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).distributorStats)}},
		{"events/{id}", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).serverEvent)}},
		{"event-kinds", idx((*Handler).eventKinds)},
		{"tenants/{t}/publishers", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).listPublishers),
			http.MethodPost: mb(distributionAdmin, (*Handler).addPublisher)}},
		{"tenants/{t}/publishers/{name}", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).getPublisher),
			http.MethodDelete: m(pathAdmin, (*Handler).removePublisher)}},
		// spec 0021: marking a publisher a source of licence keys (server administrators; spec 0030:
		// distribution admins)
		{"tenants/{t}/publishers/{name}/license-keys", map[string]rule{http.MethodPost: m(distributionAdmin, publisherLicenseKeys(true)),
			http.MethodDelete: m(distributionAdmin, publisherLicenseKeys(false))}},
		// spec 0033 phase A2: a publisher's hold (the tenant's administrators'; a distributor's
		// publisher's is a distribution admin's, phase B)
		{"tenants/{t}/publishers/{name}/hold", map[string]rule{http.MethodPut: mb(distributionAdmin, (*Handler).setPublisherHold)}},
		// spec 0033 phase B: marking a distributor's publisher (distribution admins and server
		// administrators)
		{"tenants/{t}/publishers/{name}/distributor", map[string]rule{http.MethodPut: mb(distributionAdmin, (*Handler).setPublisherDistributor)}},
		// spec 0023: a publisher's public SSH keys
		{"tenants/{t}/publishers/{name}/keys", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).listPublisherKeys),
			http.MethodPost: mb(distributionAdmin, (*Handler).addPublisherKey)}},
		{"tenants/{t}/publishers/{name}/keys/{id}", map[string]rule{http.MethodGet: m(distributionAdmin, (*Handler).getPublisherKey),
			http.MethodDelete: m(distributionAdmin, (*Handler).removePublisherKey)}},
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
		{"tenants/{t}/upstreams/{name}/credential", map[string]rule{http.MethodPost: mb(pathAdmin, (*Handler).setCredential)}},
		{"tenants/{t}/upstreams/{name}/fetch", map[string]rule{http.MethodPost: mb(pathAdmin, (*Handler).setFetch)}},
		// spec 0033 phase A2: an eager mirror's hold
		{"tenants/{t}/upstreams/{name}/hold", map[string]rule{http.MethodPut: mb(pathAdmin, (*Handler).setUpstreamHold)}},
		{"tenants/{t}/upstreams/{name}/usage", map[string]rule{http.MethodGet: m(pathAdmin, (*Handler).usageForward),
			http.MethodPost: mb(pathAdmin, (*Handler).setUsage)}},
		{"credentials", map[string]rule{http.MethodGet: m(serverAdmin, (*Handler).listCredentials)}},
		// spec 0012: licences (tenant administrators and, spec 0029, licence managers, who never make one
		// public), their relay per upstream (tenant administrators only); spec 0021: the keys
		{"tenants/{t}/license-keys", map[string]rule{http.MethodGet: m(licenseAdmin, licenseRoute((*Handler).licenseKeys))}},
		{"tenants/{t}/license-keys/{name}", map[string]rule{http.MethodGet: m(licenseAdmin, licenseRoute((*Handler).licenseKeys))}},
		{"tenants/{t}/licenses", map[string]rule{http.MethodGet: m(licenseAdmin, licenseRoute((*Handler).listLicenses)),
			http.MethodPost: {access: licenseAdmin, manage: true, raw: true, handle: licenseRoute((*Handler).importLicense)}}},
		{"tenants/{t}/licenses/{name}", map[string]rule{http.MethodGet: m(licenseAdmin, licenseRoute((*Handler).getLicense)),
			http.MethodDelete: m(licenseAdmin, licenseRoute((*Handler).removeLicense))}},
		{"tenants/{t}/licenses/{name}/public", map[string]rule{http.MethodPost: m(pathAdmin, licenseRoute(licenseVisibility(store.Public)))}},
		{"tenants/{t}/licenses/{name}/private", map[string]rule{http.MethodPost: m(licenseAdmin, licenseRoute(licenseVisibility(store.Private)))}},
		{"tenants/{t}/upstreams/{name}/licenses", map[string]rule{http.MethodGet: m(pathAdmin, licenseRoute((*Handler).licenseSync))}},
		{"tenants/{t}/upstreams/{name}/licenses/sync", map[string]rule{http.MethodPost: m(pathAdmin, licenseRoute((*Handler).syncLicenses))}},
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
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete} {
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
		problem(w, http.StatusTooManyRequests, typeRateLimited, "")
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
	if ru.manage && method != http.MethodGet {
		// spec 0035 phase 2: a write is one act, its licence warnings written once; they are answered
		// (reply) only to a caller who administers the path, and to nobody else (a publisher, a
		// promoter, a licence manager, a distribution admin), whose act writes only the events
		asked := r // the caller's rights are read only when there is a warning to answer
		ctx := context.WithValue(store.WithLicenseWarnings(r.Context()), answerWarnings{}, func() bool { return h.seesWarnings(asked, c, p) })
		r = r.WithContext(ctx)
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
	for _, k := range []string{"t", "c", "name", "ext", "g", "v", "id", "change", "hash"} {
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
	failed := func(reason string) { note(w, func(sw *statusWriter) { sw.authFailure = reason }) }
	// spec 0034: a type this server does not know is refused at every route, before anything else
	typ, typeNamed, typeOK := queryType(r)
	if !typeOK {
		problem(w, http.StatusBadRequest, typeInvalid, "type is a package type this server knows ("+strings.Join(pkgtype.Types(), ", ")+")")
		return caller{}, false
	}
	if typeNamed && !takesType(rt.pattern, r.Method) {
		problem(w, http.StatusBadRequest, typeInvalid, "this route takes no type parameter (a body names its package_type)")
		return caller{}, false
	}
	// a publisher's signed request (spec 0023): malformed, or not naming this request, is 400
	st, sig, signed, bad := signedHeaders(r)
	if bad != "" {
		problem(w, http.StatusBadRequest, typeInvalid, bad)
		return caller{}, false
	}
	var sr auth.SignedRequest
	if signed {
		var err error
		if sr, err = parseSigned(r, st, sig); err != nil {
			problem(w, http.StatusBadRequest, typeInvalid, strings.TrimPrefix(err.Error(), "auth: "))
			return caller{}, false
		}
	}
	tok, sent := bearer(r)
	if sent && tok == "" {
		failed("malformed")
		unauthorized(w)
		return caller{}, false
	}
	if !sent && !signed && ru.access != public {
		unauthorized(w) // without a token, anything not public is 401 before anything is looked up
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
		if (errors.Is(err, store.ErrDeleting) || errors.Is(err, store.ErrDeleted)) && ru.closed && c.admin {
			err = nil // spec 0019: a server administrator's view of a closed tenant
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.fail(w, err)
			return caller{}, false
		}
		// unknown and suspended tenants: 404 for everyone but server administrators (and, on a route
		// that says so, a distribution admin for a suspended one: spec 0037)
		if err != nil || t.State != store.TenantActive && !c.admin && (!ru.suspended || !c.distribution) {
			notFound(w)
			return caller{}, false
		}
		c.tenant = t
		note(w, func(sw *statusWriter) { sw.tenant = t })
		switch {
		case signed && !ru.publishers:
			if ru.access != public {
				failed("publisher_route")
				unauthorized(w) // a publisher's signed request is not a token for this route
				return caller{}, false
			}
			c = caller{tenant: t} // the public view
		case signed:
			var sf *signedFailure
			if c, err = h.identifySigned(ctx, t, sr); errors.As(err, &sf) {
				sf.write(w)
				return caller{}, false
			} else if err != nil {
				h.fail(w, err)
				return caller{}, false
			}
			// the body must be the signed one: none for a route without a body, and otherwise checked
			// at its end, before anything is written
			if !ru.body && !ru.raw || r.ContentLength == 0 {
				// no body to check as it is read: the statement must name an empty one
				if sr.Statement.SHA256 != auth.EmptySHA256 {
					problem(w, http.StatusBadRequest, typeInvalid, "the body is not the signed one: the statement names a body, the request has none")
					return caller{}, false
				}
			} else {
				r.Body = newHashedBody(r.Body, sr.Statement.SHA256, r.ContentLength)
			}
		case sent && !isServer:
			if c, err = h.identifyTenant(ctx, t, tok); err != nil {
				if errors.Is(err, errToken) {
					failed("invalid")
					unauthorized(w)
				} else {
					h.fail(w, err)
				}
				return caller{}, false
			}
		}
	case signed && ru.access != public:
		failed("publisher_route")
		unauthorized(w) // routes without a tenant take server tokens only
		return caller{}, false
	case signed:
		c = caller{}
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
	case licenseAdmin:
		if c.licenseManager {
			break // the licence service allows it license on the tenant, and nothing else
		}
		fallthrough
	case distributionAdmin:
		if ru.access == distributionAdmin && ru.distributors && c.pub != nil {
			// spec 0037: a distributor's publisher's signed request, which its service narrows to its
			// own entries; the tenant's own publisher's is refused
			if !slices.ContainsFunc(c.pub.Publishers, func(m auth.MatchedPublisher) bool { return m.Publisher.Distributor != "" }) {
				return refuse()
			}
			break
		}
		if ru.access == distributionAdmin && c.distribution && !c.admin {
			break // the services allow it distribute on the tenant, and narrow each call
		}
		if ru.access == distributionAdmin && name == "" {
			if !c.admin {
				return refuse()
			}
			break // the server's routes: a server administrator
		}
		fallthrough
	case pathAdmin:
		a, ok := c.actor()
		if !ok || h.o.Authz == nil {
			return refuse()
		}
		// a group's administrators read it (spec 0017); its writes are the tenant administrator's
		res := authz.Resource{Tenant: name, Channel: p["c"], Package: p["ext"]}
		if p["c"] == "" {
			res.PackageType = typ // spec 0034: a channel's is the channel's (the authorizer reads it)
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			res.PackageGroup = p["g"]
		}
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
	case tenantCaller:
		if !c.verified || name == "" {
			return refuse()
		}
	case pathVerbs:
		a, ok := c.actor()
		if !ok || h.o.Authz == nil {
			return refuse()
		}
		res := authz.Resource{Tenant: name, Channel: p["c"], Package: p["ext"]}
		if p["c"] == "" {
			res.PackageType = typ // spec 0034: a channel's is the channel's (the authorizer reads it)
		}
		if p["ext"] != "" && h.o.Store != nil {
			ptype := typ // a channel's name is of the channel's type
			if p["c"] != "" {
				ct, err := h.o.Store.ChannelType(ctx, c.tenant.ID, p["c"])
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					h.fail(w, err)
					return caller{}, false
				}
				if err == nil {
					ptype = ct
				}
			}
			kind, err := h.o.Store.ReservedFor(ctx, name, ptype, p["ext"]) // spec 0030: DuckDB's, the tenant's, its upstreams'
			if err != nil {
				h.fail(w, err)
				return caller{}, false
			}
			res.Reserved = kind != ""
		}
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
		if !allowed && ru.distributors && c.pub != nil {
			for _, m := range c.pub.Publishers {
				allowed = allowed || m.Publisher.Distributor != ""
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
	// spec 0034: on a channel's routes and an upstream's the type is the channel's; a ?type= that
	// differs is refused (once the caller may see the route)
	if typeNamed && onTenant && h.o.Store != nil {
		var cur string
		var err error
		switch {
		case p["c"] != "":
			cur, err = h.o.Store.ChannelType(ctx, c.tenant.ID, p["c"])
		case p["name"] != "" && strings.HasPrefix(rt.pattern, "tenants/{t}/upstreams/{name}"):
			cur, err = h.o.Store.UpstreamChannelType(ctx, c.tenant.ID, p["name"])
		}
		switch {
		case errors.Is(err, store.ErrNotFound): // the route answers it
		case err != nil:
			h.fail(w, err)
			return caller{}, false
		case cur != "" && !pkgtype.Known(cur):
			notFound(w) // a channel (an upstream's home) of a type this server does not know: none (fail closed)
			return caller{}, false
		case cur != "" && cur != typ:
			problem(w, http.StatusBadRequest, typeInvalid, "the type is the channel's ("+cur+"), not "+typ)
			return caller{}, false
		}
	}
	return c, true
}

// takesType reports whether a route takes ?type= (spec 0034's vocabulary): the tenant-level routes
// keyed or filtered by a name (licences and their keys, the server's and a tenant's reserved names,
// a package's attachments, the catalogue's routes, the grants and the groups filtered by extension,
// statistics), and a channel's and an upstream's routes, where it must be the channel's. Anywhere
// else a type would be ignored, so it is refused: a creation, a grant or a member change names its
// type in its body.
func takesType(pattern, method string) bool {
	get := method == http.MethodGet || method == http.MethodHead
	switch {
	case strings.Contains(pattern, "/channels/{c}"), strings.HasPrefix(pattern, "tenants/{t}/upstreams/{name}"),
		pattern == "reserved-names", pattern == "reserved-names/{name}", strings.HasPrefix(pattern, "tenants/{t}/reserved-names"),
		strings.HasPrefix(pattern, "tenants/{t}/extensions/{ext}/attachments"), strings.HasPrefix(pattern, "tenants/{t}/catalogue"),
		strings.HasPrefix(pattern, "tenants/{t}/stats/"), strings.HasPrefix(pattern, "tenants/{t}/licenses"),
		strings.HasPrefix(pattern, "tenants/{t}/license-keys"), pattern == "distributor-stats/downloads":
		return true
	case pattern == "tenants/{t}/grants", pattern == "tenants/{t}/extension-groups":
		return get
	}
	return false
}

// queryType reads a request's ?type= (spec 0034): the default when the query names none; named says
// it named one; ok is false for a type this server does not know, or one named twice.
func queryType(r *http.Request) (t string, named, ok bool) {
	vs, named := r.URL.Query()["type"]
	if !named {
		return pkgtype.Default, false, true
	}
	if len(vs) != 1 {
		return "", true, false
	}
	t, err := pkgtype.Parse(vs[0])
	return t, true, err == nil
}

// reqType is a request's package type: its ?type= (decide refused one it does not know), else the
// default.
func reqType(r *http.Request) string {
	t, _, ok := queryType(r)
	if !ok {
		return pkgtype.Default
	}
	return t
}

// shownType is a package type as an answer shows it (spec 0034): omitted ("") when it is the
// default, so what an existing caller reads does not change.
func shownType(t string) string {
	if t == pkgtype.Default {
		return ""
	}
	return t
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
	// refused (a 404 that stands for forbidden), with what the refused act was when its service says
	// (refusal: spec 0037, a write on a distributor's licence entry).
	authFailure string
	refused     bool
	refusal     map[string]any
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
		f := map[string]any{"route": route, "status": sw.status}
		for k, v := range sw.refusal {
			f[k] = v
		}
		h.o.Events.Refusal(r.Context(), sw.tenant.ID, actor, "authz.refused", audit.Refused, subject, f)
	case sw.status >= 500:
		h.o.Events.Refusal(r.Context(), sw.tenant.ID, actor, "request.failed", audit.Failed, subject,
			map[string]any{"route": route, "status": sw.status})
	}
}
