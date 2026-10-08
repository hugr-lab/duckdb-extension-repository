package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Options configure a Handler.
type Options struct {
	MaxDownloads          int
	MaxDownloadsPerClient int
	MinRate               int64 // bytes per second
	WriteIdleTimeout      time.Duration
	TrustedProxies        []netip.Prefix
	Log                   *slog.Logger
	// PublicURL gives each tenant's canonical audience, <public_url>/<tenant> (phase 2).
	PublicURL string
	// Verifier verifies Bearer tokens; nil: every caller is anonymous.
	Verifier *auth.Verifier
	// Server recognises server tokens (spec 0007), which the DuckDB routes treat as no token.
	Server *auth.Server
	// Providers are the trusted-publishing providers (spec 0008): tenant records at their URLs are
	// never used, and their tokens are no token on the DuckDB routes.
	Providers auth.Providers
	// Auths and Snapshots are shared with the API (created here when nil).
	Auths     *auth.TenantAuths
	Snapshots *release.Snapshots
	// API serves /api/ on https requests (spec 0007); nil: /api/ answers 404.
	API http.Handler
}

// Handler serves the DuckDB routes and health.
type Handler struct {
	st    *store.Store
	keys  *keys.Service
	blob  *blob.Service
	rv    *resolver
	o     Options
	log   *slog.Logger
	slots chan struct{}

	mu        sync.Mutex
	perClient map[string]int

	failures *auth.FailureLog

	ready         atomic.Bool
	stopping      atomic.Bool
	publicDomains atomic.Pointer[map[string]bool]
}

// NewHandler returns a handler; it is not ready until SetReady(true).
func NewHandler(st *store.Store, ks *keys.Service, bs *blob.Service, o Options) *Handler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Auths == nil {
		o.Auths = &auth.TenantAuths{Store: st}
	}
	if o.Snapshots == nil {
		o.Snapshots = &release.Snapshots{Store: st}
	}
	h := &Handler{st: st, keys: ks, blob: bs, rv: newResolver(st, o.Snapshots), o: o, log: o.Log,
		slots: make(chan struct{}, o.MaxDownloads), perClient: map[string]int{}, failures: &auth.FailureLog{Log: o.Log}}
	empty := map[string]bool{}
	h.publicDomains.Store(&empty)
	return h
}

// SetReady sets what /readyz answers, until SetStopping.
func (h *Handler) SetReady(ok bool) { h.ready.Store(ok) }

// SetStopping makes /readyz fail for good: the server is draining.
func (h *Handler) SetStopping() { h.stopping.Store(true) }

// SetPublicDomains records the storage domains found public: their tenants are refused.
func (h *Handler) SetPublicDomains(names []string) {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	h.publicDomains.Store(&m)
}

type ctxKey int

const (
	schemeKey ctxKey = iota
	listenerKey
)

// scheme is the request's scheme as the listener decided it ("https" or "http").
func scheme(r *http.Request) string {
	if s, ok := r.Context().Value(schemeKey).(string); ok {
		return s
	}
	return "http"
}

// Fixed answers: identical bytes for every path they stand for.
var (
	body401 = []byte("unauthorized\n")
	body404 = []byte("not found\n")
	body400 = []byte("bad request\n")
	body405 = []byte("method not allowed\n")
	body503 = []byte("unavailable\n")
)

func plain(w http.ResponseWriter, status int, body []byte) {
	hd := w.Header()
	hd.Set("Content-Type", "text/plain; charset=utf-8")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Vary", "Authorization")
	hd.Set("Content-Length", strconv.Itoa(len(body)))
	switch status {
	case http.StatusUnauthorized:
		hd.Set("WWW-Authenticate", `Bearer realm="kista"`)
	case http.StatusMethodNotAllowed:
		hd.Set("Allow", "GET, HEAD")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) missing(w http.ResponseWriter) { plain(w, http.StatusUnauthorized, body401) }
func notFound(w http.ResponseWriter)             { plain(w, http.StatusNotFound, body404) }

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	lw := &logWriter{ResponseWriter: w, status: http.StatusOK}
	aborted := true
	defer func() {
		h.log.Info("request", "method", r.Method, "path", r.URL.EscapedPath(), "status", lw.status, "bytes", lw.bytes,
			"duration", time.Since(start).Round(time.Millisecond), "client", h.clientAddr(r), "scheme", scheme(r), "listener", listenerOf(r),
			"aborted", aborted)
	}()
	defer func() { aborted = false }() // skipped by a panic: an aborted response
	if strings.HasPrefix(r.URL.EscapedPath(), "/api/") {
		// the API (spec 0007): https only, its own rules for methods, queries and bodies
		if scheme(r) != "https" || h.o.API == nil {
			notFound(lw)
			return
		}
		h.o.API.ServeHTTP(lw, r.WithContext(api.WithClient(r.Context(), h.clientAddr(r))))
		return
	}
	rt := parseRoute(r)
	if rt.kind == routeNone {
		notFound(lw)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		plain(lw, http.StatusMethodNotAllowed, body405)
		return
	}
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		plain(lw, http.StatusBadRequest, body400)
		return
	}
	switch rt.kind {
	case routeHealthz:
		plain(lw, http.StatusOK, []byte("ok\n"))
	case routeReadyz:
		if h.ready.Load() && !h.stopping.Load() {
			plain(lw, http.StatusOK, []byte("ready\n"))
		} else {
			plain(lw, http.StatusServiceUnavailable, body503)
		}
	case routeWellKnown:
		h.wellKnown(lw, r, rt)
	default:
		h.binary(lw, r, rt)
	}
}

func (h *Handler) wellKnown(w http.ResponseWriter, r *http.Request, rt route) {
	sc, err := h.st.GetServeChannel(r.Context(), rt.tenant, rt.channel)
	if err != nil || sc.Tenant.State != store.TenantActive {
		h.storeError(w, err)
		return
	}
	doc, _, err := h.keys.WellKnownOf(r.Context(), sc.Channel)
	if errors.Is(err, keys.ErrNoKeys) {
		notFound(w)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	sum := sha256.Sum256(doc)
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "public, no-cache")
	hd.Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(doc)))
}

// storeError answers a failed channel lookup: a missing tenant or channel is 404, anything else 503.
func (h *Handler) storeError(w http.ResponseWriter, err error) {
	if err == nil || errors.Is(err, store.ErrNotFound) {
		notFound(w)
		return
	}
	h.fail(w, err)
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	h.log.Error("serve: request failed", "error", err)
	plain(w, http.StatusServiceUnavailable, body503)
}

func (h *Handler) binary(w http.ResponseWriter, r *http.Request, rt route) {
	ctx := r.Context()
	sc, err := h.st.GetServeChannel(ctx, rt.tenant, rt.channel)
	if err != nil || sc.Tenant.State != store.TenantActive {
		h.storeError(w, err)
		return
	}
	if sc.Channel.Kind != store.ChannelSigned {
		notFound(w) // passthrough channels are fed by upstreams (spec 0009)
		return
	}
	// the decision comes from the path: a grant names the tenant, channel or extension, all known
	// before anything is resolved; a caller without install resolves public releases only
	v, tokenValid, err := h.decide(r, sc, rt.name)
	if err != nil {
		h.fail(w, err)
		return
	}
	res, err := h.rv.resolve(ctx, sc, rt, v)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !res.found {
		if tokenValid {
			notFound(w) // a valid token without the grant, or a path that does not exist: the same
		} else {
			h.missing(w)
		}
		return
	}
	// after the decision: conditions of the tenant or the body
	domain := sc.Tenant.StorageDomain
	if !h.blob.HasDomain(domain) || (*h.publicDomains.Load())[domain] {
		h.log.Error("serve: the tenant's storage domain is not configured or is public", "tenant", sc.Tenant.Name, "domain", domain)
		plain(w, http.StatusServiceUnavailable, body503)
		return
	}
	bh, err := hashOf(res.cand.BodyHash)
	if err != nil {
		h.fail(w, err)
		return
	}
	rec, err := h.blob.Record(ctx, domain, bh)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !rec.CorruptAt.IsZero() {
		h.log.Error("serve: the release's stored stream is marked corrupt", "tenant", sc.Tenant.Name, "release", res.cand.ID)
		plain(w, http.StatusServiceUnavailable, body503)
		return
	}
	var f *blob.File
	if rt.gz {
		f, err = h.blob.OpenGzip(ctx, rec, res.sig)
	} else {
		f, err = h.blob.OpenPlain(ctx, rec, res.sig)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", f.ContentType)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("ETag", f.ETag)
	if res.cand.Visibility == store.Public {
		hd.Set("Cache-Control", "public, no-cache, no-transform")
	} else {
		hd.Set("Cache-Control", "private, no-store, no-transform")
	}
	if scheme(r) == "https" {
		hd.Set("Vary", "Authorization")
	}
	notModified := etagMatches(r.Header.Get("If-None-Match"), f.ETag)
	if r.Method == http.MethodGet && !notModified {
		release, ok := h.acquire(h.clientAddr(r))
		if !ok {
			for _, k := range []string{"ETag", "Content-Type"} {
				hd.Del(k)
			}
			hd.Set("Retry-After", "5")
			plain(w, http.StatusServiceUnavailable, body503)
			return
		}
		defer release()
	}
	rd := &trackReader{Reader: f.NewReader(ctx)}
	defer func() { _ = rd.Close() }()
	out := h.rateWriter(&errHeaders{ResponseWriter: w})
	if rt.gz {
		if strings.Contains(r.Header.Get("Range"), ",") {
			r.Header.Del("Range") // one range at most: several get the whole file (RFC 9110)
		}
		// no write preconditions on a read-only resource: an If-Match never yields a 412 here
		r.Header.Del("If-Match")
		r.Header.Del("If-Unmodified-Since")
		http.ServeContent(out, r, "", time.Time{}, rd)
	} else {
		h.servePlain(out, r, f, rd, notModified)
	}
	if rd.err != nil {
		if errors.Is(rd.err, blob.ErrCorrupt) {
			h.log.Error("serve: a stored stream failed verification mid-response; aborting", "release", res.cand.ID)
		}
		panic(http.ErrAbortHandler) // in the handler goroutine: never a clean short body
	}
}

// bearer returns the request's Bearer token: only on https, only from exactly one Authorization
// header.
func bearer(r *http.Request) string {
	if scheme(r) != "https" {
		return "" // a token over plain http is never used
	}
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return ""
	}
	sch, tok, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(sch, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// decide returns what the caller may see in the channel, and whether it presented a valid token.
func (h *Handler) decide(r *http.Request, sc store.ServeChannel, name string) (view, bool, error) {
	tok := bearer(r)
	// a publisher's API key is never looked up here (spec 0008): it is no token
	if tok == "" || h.o.Verifier == nil || h.o.Server.IsServerToken(tok) || auth.IsAPIKey(tok) {
		return viewPublic, false, nil
	}
	ta, err := h.tenantAuth(r.Context(), sc.Tenant)
	if err != nil {
		h.log.Error("serve: reading a tenant's issuers and grants", "tenant", sc.Tenant.Name, "error", err)
		return viewPublic, false, nil // no token: public releases are still served
	}
	ta, canonical := auth.ForTenant(ta, h.o.PublicURL, sc.Tenant.Name, h.o.Providers)
	p, _, err := h.o.Verifier.Verify(r.Context(), ta, canonical, tok)
	if err != nil {
		h.failures.Record(sc.Tenant, "serve", err)
		return viewPublic, false, nil // an invalid token is no token: public releases are still served
	}
	if auth.Allows(p, ta.Grants, sc.Channel.ID, name, store.VerbInstall) {
		return viewAll, true, nil
	}
	return viewPublic, true, nil
}

func (h *Handler) tenantAuth(ctx context.Context, t store.Tenant) (store.TenantAuth, error) {
	return h.o.Auths.Get(ctx, t)
}

// servePlain sends the plain name whole: no ranges, 304 on a matching If-None-Match.
func (h *Handler) servePlain(w http.ResponseWriter, r *http.Request, f *blob.File, rd io.Reader, notModified bool) {
	if notModified {
		w.Header().Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, rd)
}

func etagMatches(inm, etag string) bool {
	if inm == "" {
		return false
	}
	for _, t := range strings.Split(inm, ",") {
		t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "W/"))
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

func hashOf(s string) (extfile.BodyHash, error) {
	var h extfile.BodyHash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(h) {
		return h, errors.New("serve: a stored body hash is malformed")
	}
	copy(h[:], b)
	return h, nil
}

// trackReader keeps the first error other than EOF, so the handler can abort after ServeContent
// (which ignores copy errors) returns.
type trackReader struct {
	blob.Reader
	err error
}

func (t *trackReader) Read(p []byte) (int, error) {
	n, err := t.Reader.Read(p)
	if err != nil && err != io.EOF && t.err == nil {
		t.err = err
	}
	return n, err
}

// acquire takes a download slot for a client; ok is false when the replica or the client is at its
// limit.
func (h *Handler) acquire(client string) (func(), bool) {
	h.mu.Lock()
	if h.perClient[client] >= h.o.MaxDownloadsPerClient {
		h.mu.Unlock()
		return nil, false
	}
	select {
	case h.slots <- struct{}{}:
	default:
		h.mu.Unlock()
		return nil, false
	}
	h.perClient[client]++
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		if h.perClient[client]--; h.perClient[client] <= 0 {
			delete(h.perClient, client)
		}
		<-h.slots
		h.mu.Unlock()
	}, true
}

// rateWriter moves the write deadline forward only after every MinRate × WriteIdleTimeout bytes,
// so a client must read at least MinRate on average.
type rateWriter struct {
	http.ResponseWriter
	rc      *http.ResponseController
	idle    time.Duration
	chunk   int64
	written int64
	next    int64
}

func (h *Handler) rateWriter(w http.ResponseWriter) http.ResponseWriter {
	rw := &rateWriter{ResponseWriter: w, rc: http.NewResponseController(w), idle: h.o.WriteIdleTimeout,
		chunk: h.o.MinRate * int64(h.o.WriteIdleTimeout/time.Second)}
	if rw.chunk <= 0 {
		rw.chunk = 1 << 20
	}
	rw.next = rw.chunk
	_ = rw.rc.SetWriteDeadline(time.Now().Add(rw.idle))
	return rw
}

func (rw *rateWriter) Write(p []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(p)
	rw.written += int64(n)
	if rw.written >= rw.next {
		rw.next = rw.written + rw.chunk
		_ = rw.rc.SetWriteDeadline(time.Now().Add(rw.idle))
	}
	return n, err
}

func (rw *rateWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// errHeaders restores the headers of a non-success answer that ServeContent writes itself (412,
// 416): Go removes Cache-Control and ETag on those.
type errHeaders struct{ http.ResponseWriter }

func (e *errHeaders) WriteHeader(code int) {
	if code >= 400 {
		e.Header().Set("Cache-Control", "private, no-store")
		e.Header().Set("Vary", "Authorization")
		e.Header().Del("ETag")
	}
	e.ResponseWriter.WriteHeader(code)
}

func (e *errHeaders) Unwrap() http.ResponseWriter { return e.ResponseWriter }

// logWriter records the status and bytes for the access log.
type logWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (l *logWriter) WriteHeader(code int) {
	if !l.wrote {
		l.status, l.wrote = code, true
	}
	l.ResponseWriter.WriteHeader(code)
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.wrote = true
	n, err := l.ResponseWriter.Write(p)
	l.bytes += int64(n)
	return n, err
}

func (l *logWriter) Unwrap() http.ResponseWriter { return l.ResponseWriter }

// clientAddr is the connection's address, or, behind a trusted proxy, the rightmost X-Forwarded-For
// entry that is not a trusted proxy.
func (h *Handler) clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !h.trusted(peer) {
		return host
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := parseHop(strings.TrimSpace(hops[i]))
		if err != nil {
			return host
		}
		if !h.trusted(a) {
			return a.String()
		}
	}
	return host
}

// parseHop parses an X-Forwarded-For entry: an address, or an address with a port (some proxies,
// such as Azure Application Gateway, append it).
func parseHop(s string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(s); err == nil {
		return a, nil
	}
	ap, err := netip.ParseAddrPort(s)
	return ap.Addr(), err
}

func (h *Handler) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range h.o.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// withScheme marks a request's scheme for the handler.
func withScheme(ctx context.Context, s string) context.Context {
	return context.WithValue(ctx, schemeKey, s)
}

// withConn marks a request's scheme and the listener it came through.
func withConn(ctx context.Context, scheme, listener string) context.Context {
	return context.WithValue(withScheme(ctx, scheme), listenerKey, listener)
}

func listenerOf(r *http.Request) string {
	l, _ := r.Context().Value(listenerKey).(string)
	return l
}

// WithHTTPS marks a request as having come through an https listener (for callers that embed the
// handler without kista's listeners, and tests).
func WithHTTPS(r *http.Request) *http.Request { return r.WithContext(withScheme(r.Context(), "https")) }
