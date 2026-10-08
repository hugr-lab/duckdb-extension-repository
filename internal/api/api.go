// Package api is /api/v1 (spec 0007): the index of what a channel serves to the caller, and (later
// phases) management. kista serve hands /api/ to it on https requests only.
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
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
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
}

// Handler serves /api/v1.
type Handler struct {
	o        Options
	limiter  *limiter
	failures *auth.FailureLog
}

// New returns a handler.
func New(o Options) *Handler {
	if o.Rate == 0 {
		o.Rate, o.Burst = 20, 40
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Handler{o: o, limiter: newLimiter(o.Rate, o.Burst), failures: &auth.FailureLog{Log: o.Log}}
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
	typeTooMany      = "too-many-requests"
	typeUnavailable  = "unavailable"
)

var titles = map[string]string{typeUnauthorized: "Unauthorized", typeNotFound: "Not Found", typeMethod: "Method Not Allowed",
	typeInvalid: "Invalid", typeTooMany: "Too Many Requests", typeUnavailable: "Service Unavailable"}

// problem writes application/problem+json. 401 and 404 have constant bytes.
func problem(w http.ResponseWriter, status int, typ, detail string) {
	body := map[string]any{"type": "urn:kista:problem:" + typ, "title": titles[typ], "status": status}
	if detail != "" && status != http.StatusUnauthorized && status != http.StatusNotFound {
		body["detail"] = detail
	}
	b, _ := json.Marshal(body)
	hd := w.Header()
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

func notFound(w http.ResponseWriter) { problem(w, http.StatusNotFound, typeNotFound, "") }

// answer writes a JSON answer with an ETag over its bytes (304 on a match).
func answer(w http.ResponseWriter, r *http.Request, v any, anonymous bool) {
	b, err := json.Marshal(v)
	if err != nil {
		problem(w, http.StatusServiceUnavailable, typeUnavailable, "")
		return
	}
	sum := sha256.Sum256(b)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("ETag", etag)
	hd.Set("Vary", "Authorization")
	if anonymous {
		hd.Set("Cache-Control", "public, no-cache")
	} else {
		hd.Set("Cache-Control", "private, no-cache")
	}
	for _, t := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if t = strings.TrimSpace(t); t == "*" || strings.TrimPrefix(t, "W/") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
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

// --- routing ---

// ServeHTTP implements http.Handler. kista serve calls it for /api/ on https requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow(client(r)) {
		w.Header().Set("Retry-After", h.limiter.retryAfter())
		problem(w, http.StatusTooManyRequests, typeTooMany, "")
		return
	}
	p := r.URL.EscapedPath()
	rest, ok := strings.CutPrefix(p, "/api/v1/")
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
	if !known(segs) {
		notFound(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		problem(w, http.StatusMethodNotAllowed, typeMethod, "")
		return
	}
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		problem(w, http.StatusBadRequest, typeInvalid, "a GET has no body")
		return
	}
	if len(segs) == 1 {
		answer(w, r, map[string]string{"kista": h.o.KistaVersion, "api": Version}, true)
		return
	}
	h.tenant(w, r, segs[1], segs[2:])
}

// known reports whether a path has the shape of a route, so that a method on an unknown path is
// 404 and not 405.
func known(segs []string) bool {
	if len(segs) == 1 {
		return segs[0] == "info"
	}
	if len(segs) < 3 || segs[0] != "tenants" {
		return false
	}
	rest := segs[2:]
	switch {
	case len(rest) == 1:
		return rest[0] == "whoami" || rest[0] == "channels"
	case rest[0] != "channels":
		return false
	case len(rest) == 2:
		return true
	case len(rest) == 3, len(rest) == 4:
		return rest[2] == "extensions"
	case len(rest) == 6:
		return rest[2] == "extensions" && rest[4] == "versions"
	}
	return false
}

// caller is who asks: anonymous, or a tenant token's principals.
type caller struct {
	principals auth.Principals
	ta         store.TenantAuth
	tenant     store.Tenant
}

func (c caller) anonymous() bool { return c.principals == nil }

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

// identify verifies the request's token against the tenant (spec 0006). A token that is sent but not
// valid is an error (401 on the API, unlike the DuckDB routes); no token is anonymous.
func (h *Handler) identify(r *http.Request, t store.Tenant) (caller, error) {
	c := caller{tenant: t}
	tok, sent := bearer(r)
	if !sent {
		return c, nil
	}
	if tok == "" || h.o.Verifier == nil {
		return c, errToken
	}
	ta, err := h.o.Auths.Get(r.Context(), t)
	if err != nil {
		return c, err
	}
	ta, canonical := auth.ForTenant(ta, h.o.PublicURL, t.Name)
	pr, _, err := h.o.Verifier.Verify(r.Context(), ta, canonical, tok)
	if err != nil {
		h.failures.Record(t, "api", err)
		return c, errToken
	}
	c.principals, c.ta = pr, ta
	return c, nil
}

var errToken = errors.New("api: the token is not valid")

func (h *Handler) tenant(w http.ResponseWriter, r *http.Request, name string, rest []string) {
	ctx := r.Context()
	t, err := h.o.Store.GetTenant(ctx, name)
	if err != nil || t.State != store.TenantActive {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.fail(w, err)
			return
		}
		notFound(w) // suspended tenants: 404 for everyone (server administrators come in phase 2)
		return
	}
	c, err := h.identify(r, t)
	if errors.Is(err, errToken) {
		problem(w, http.StatusUnauthorized, typeUnauthorized, "")
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	switch {
	case len(rest) == 1 && rest[0] == "whoami":
		if c.anonymous() {
			problem(w, http.StatusUnauthorized, typeUnauthorized, "")
			return
		}
		answer(w, r, whoami(c), false)
	case len(rest) == 1 && rest[0] == "channels":
		h.channels(w, r, c)
	case len(rest) >= 2 && rest[0] == "channels":
		h.channel(w, r, c, rest[1], rest[2:])
	default:
		notFound(w)
	}
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	h.o.Log.Error("api: request failed", "error", err)
	problem(w, http.StatusServiceUnavailable, typeUnavailable, "")
}
