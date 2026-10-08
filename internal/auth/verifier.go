package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	neturl "net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Fetcher gets a URL on a tenant's behalf (the egress client).
type Fetcher interface {
	Get(ctx context.Context, url string) ([]byte, http.Header, error)
}

// ErrToken marks a token that is not valid now. Callers treat it as no token.
var ErrToken = errors.New("auth: no valid token")

// Reason classes for logs (never the token or its claims).
const (
	ReasonMalformed = "malformed"
	ReasonIssuer    = "unknown issuer"
	ReasonKey       = "no key"
	ReasonSignature = "signature"
	ReasonTime      = "time"
	ReasonAudience  = "audience"
	ReasonClaims    = "required claims"
)

// Failure is why a token is not valid.
type Failure struct {
	Reason string
	Issuer string // the issuer record's name, if one matched
}

func (f *Failure) Error() string { return "auth: no valid token (" + f.Reason + ")" }
func (f *Failure) Unwrap() error { return ErrToken }

// Key is one principal: an issuer record, a kind, a value (empty for issuer:).
type Key struct{ IssuerID, Kind, Value string }

// Principals are a caller's principals.
type Principals map[Key]bool

const (
	skew          = 60 * time.Second
	minJWKSTTL    = 5 * time.Minute
	maxJWKSTTL    = 24 * time.Hour
	defJWKSTTL    = time.Hour
	refreshEvery  = time.Minute
	discoveryTTL  = 6 * time.Hour
	maxKeys       = 64
	maxClaimLen   = 256
	maxClaimItems = 256
)

// Verifier verifies tokens. JWKS documents are cached per URL (shared by records with one URL);
// verification is per issuer record.
type Verifier struct {
	Fetch Fetcher
	Now   func() time.Time

	mu    sync.Mutex
	jwks  map[string]*jwksEntry
	disco map[string]*discoEntry
}

// jwksEntry is one JWKS URL's keys. A refresh runs in its own goroutine on a detached context, so
// no request (and no client closing its connection) can cancel it.
type jwksEntry struct {
	keys        map[string]jwk
	fetched     bool      // keys hold a successful fetch
	expires     time.Time // after expires+staleGrace the keys are not used at all
	lastAttempt time.Time
	done        chan struct{} // non-nil while a refresh runs
}

type discoEntry struct {
	jwksURI     string
	fetched     bool
	expires     time.Time
	lastAttempt time.Time
	done        chan struct{}
}

// staleGrace is how long past their expiry keys still verify while refreshes fail: a key the
// issuer removed stops working at the latest then.
const staleGrace = time.Hour

const maxEntries = 1024

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Verify checks a token against a tenant's issuers and audiences (the canonical one first) and
// returns the caller's principals and the issuer record's name. Any failure is a *Failure.
func (v *Verifier) Verify(ctx context.Context, ta store.TenantAuth, canonical string, token string) (Principals, string, error) {
	t, err := parse(token)
	if err != nil {
		return nil, "", &Failure{Reason: ReasonMalformed}
	}
	iss, _ := t.claims["iss"].(string)
	var rec *store.Issuer
	for i := range ta.Issuers {
		if ta.Issuers[i].URL == iss {
			rec = &ta.Issuers[i]
		}
	}
	if rec == nil || iss == "" {
		return nil, "", &Failure{Reason: ReasonIssuer}
	}
	fail := func(r string) (Principals, string, error) {
		return nil, rec.Name, &Failure{Reason: r, Issuer: rec.Name}
	}
	if !slices.Contains(rec.Algorithms, t.alg) {
		return fail(ReasonSignature)
	}
	k, ok := v.key(ctx, *rec, t.kid)
	if !ok {
		return fail(ReasonKey)
	}
	if err := t.verify(k); err != nil {
		return fail(ReasonSignature)
	}
	now := v.now()
	exp, okE := numericDate(t.claims["exp"])
	iat, okI := numericDate(t.claims["iat"])
	if !okE || !okI || !now.Before(exp.Add(skew)) || iat.After(now.Add(skew)) || exp.Sub(iat) > lifetime(*rec) {
		return fail(ReasonTime)
	}
	if nbfV, ok := t.claims["nbf"]; ok {
		nbf, ok := numericDate(nbfV)
		if !ok || nbf.After(now.Add(skew)) {
			return fail(ReasonTime)
		}
	}
	if !audienceOK(t.claims["aud"], canonical, ta.Audiences) {
		return fail(ReasonAudience)
	}
	for c, want := range rec.RequiredClaims {
		if got, ok := t.claims[c].(string); !ok || got != want {
			return fail(ReasonClaims)
		}
	}
	p := Principals{{IssuerID: rec.ID, Kind: store.PrincipalIssuer}: true}
	if sub, ok := t.claims["sub"].(string); ok && cleanValue(sub) {
		p[Key{rec.ID, store.PrincipalSubject, sub}] = true
	}
	for kind, path := range map[string][]string{store.PrincipalRole: rec.RolesClaim, store.PrincipalGroup: rec.GroupsClaim,
		store.PrincipalClient: rec.ClientClaim} {
		for _, val := range claimValues(t.claims, path) {
			p[Key{rec.ID, kind, val}] = true
		}
	}
	return p, rec.Name, nil
}

func lifetime(rec store.Issuer) time.Duration {
	if rec.MaxTokenLifetime <= 0 {
		return 24 * time.Hour
	}
	return rec.MaxTokenLifetime
}

func numericDate(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || f < 0 || f > 1e11 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

func audienceOK(aud any, canonical string, assigned []string) bool {
	ok := func(a string) bool { return a != "" && (a == canonical || slices.Contains(assigned, a)) }
	switch x := aud.(type) {
	case string:
		return ok(x)
	case []any:
		for _, e := range x {
			if s, isStr := e.(string); isStr && ok(s) {
				return true
			}
		}
	}
	return false
}

func cleanValue(s string) bool {
	if s == "" || len(s) > maxClaimLen {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) { // control and format (bidi, zero-width)
			return false
		}
	}
	return true
}

// claimValues follows a claim path (a list of keys; a key may contain dots) to a string or an array
// of strings; non-strings and unclean values are dropped.
func claimValues(claims map[string]any, path []string) []string {
	if len(path) == 0 {
		return nil
	}
	var cur any = claims
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	var out []string
	switch x := cur.(type) {
	case string:
		if cleanValue(x) {
			out = append(out, x)
		}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && cleanValue(s) && len(out) < maxClaimItems {
				out = append(out, s)
			}
		}
	}
	return out
}

// key finds a record's key by kid. A refresh (on an unknown kid or expired keys) starts at most once
// a minute per URL, in the background; only the very first fetch of a URL is waited for (bounded
// by the request), and nobody waits for a refresh another request started.
func (v *Verifier) key(ctx context.Context, rec store.Issuer, kid string) (jwk, bool) {
	url := rec.JWKSURI
	if url == "" {
		var ok bool
		if url, ok = v.discover(ctx, rec.URL); !ok {
			return jwk{}, false
		}
	}
	v.mu.Lock()
	if v.jwks == nil || len(v.jwks) >= maxEntries {
		v.jwks = map[string]*jwksEntry{}
	}
	e := v.jwks[url]
	if e == nil {
		e = &jwksEntry{}
		v.jwks[url] = e
	}
	now := v.now()
	_, known := e.keys[kid]
	fresh := e.fetched && now.Before(e.expires)
	if (!known || !fresh) && e.done == nil && now.Sub(e.lastAttempt) >= refreshEvery {
		e.lastAttempt, e.done = now, make(chan struct{})
		go v.refreshJWKS(url, e)
	}
	wait := e.done
	first := !e.fetched
	v.mu.Unlock()

	if first && wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return jwk{}, false
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	k, known := e.keys[kid]
	if !known || !e.fetched || !v.now().Before(e.expires.Add(staleGrace)) {
		return jwk{}, false
	}
	return k, true
}

func (v *Verifier) refreshJWKS(url string, e *jwksEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	keys, ttl, err := v.fetchJWKS(ctx, url)
	v.mu.Lock()
	defer v.mu.Unlock()
	if err == nil {
		e.keys, e.fetched, e.expires = keys, true, v.now().Add(ttl)
	}
	close(e.done)
	e.done = nil
}

func (v *Verifier) fetchJWKS(ctx context.Context, url string) (map[string]jwk, time.Duration, error) {
	b, hdr, err := v.Fetch.Get(ctx, url)
	if err != nil {
		return nil, 0, err
	}
	var doc map[string]any
	if err := strictJSON(b, &doc); err != nil {
		return nil, 0, fmt.Errorf("auth: the JWKS is not valid JSON")
	}
	list, _ := doc["keys"].([]any)
	if len(list) > maxKeys {
		return nil, 0, fmt.Errorf("auth: the JWKS has more than %d keys", maxKeys)
	}
	keys := map[string]jwk{}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		str := func(k string) string { s, _ := m[k].(string); return s } // exact, case-sensitive member names
		r := rawJWK{Kty: str("kty"), Kid: str("kid"), Use: str("use"), Alg: str("alg"), N: str("n"), E: str("e"),
			Crv: str("crv"), X: str("x"), Y: str("y")}
		if _, has := m["d"]; has {
			r.D = "present"
		}
		if k, err := parseJWK(r); err == nil {
			if _, dup := keys[k.kid]; !dup {
				keys[k.kid] = k
			}
		}
	}
	return keys, cacheTTL(hdr), nil
}

// cacheTTL reads max-age from Cache-Control, bounded to 5m..24h (1h without one).
func cacheTTL(h http.Header) time.Duration {
	for _, d := range strings.Split(h.Get("Cache-Control"), ",") {
		d = strings.TrimSpace(strings.ToLower(d))
		if v, ok := strings.CutPrefix(d, "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				return min(max(time.Duration(n)*time.Second, minJWKSTTL), maxJWKSTTL)
			}
		}
	}
	return defJWKSTTL
}

// discover returns an issuer's jwks_uri from its OIDC discovery document, cached for 6 hours and
// refreshed like JWKS: in the background, at most once a minute, the first fetch waited for.
func (v *Verifier) discover(ctx context.Context, issuer string) (string, bool) {
	v.mu.Lock()
	if v.disco == nil || len(v.disco) >= maxEntries {
		v.disco = map[string]*discoEntry{}
	}
	e := v.disco[issuer]
	if e == nil {
		e = &discoEntry{}
		v.disco[issuer] = e
	}
	now := v.now()
	if (!e.fetched || !now.Before(e.expires)) && e.done == nil && now.Sub(e.lastAttempt) >= refreshEvery {
		e.lastAttempt, e.done = now, make(chan struct{})
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			uri, err := Discover(cctx, v.Fetch, issuer)
			v.mu.Lock()
			defer v.mu.Unlock()
			if err == nil {
				e.jwksURI, e.fetched, e.expires = uri, true, v.now().Add(discoveryTTL)
			}
			close(e.done)
			e.done = nil
		}()
	}
	wait, first := e.done, !e.fetched
	v.mu.Unlock()
	if first && wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return "", false
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return e.jwksURI, e.fetched
}

// Discover reads an issuer's OIDC discovery document: its issuer must be exactly the record's URL,
// and its jwks_uri https.
func Discover(ctx context.Context, f Fetcher, issuer string) (string, error) {
	b, _, err := f.Get(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration")
	if err != nil {
		return "", err
	}
	var doc map[string]any
	if err := strictJSON(b, &doc); err != nil {
		return "", errors.New("auth: the discovery document is not valid JSON")
	}
	if got, _ := doc["issuer"].(string); got != issuer {
		return "", errors.New("auth: the discovery document names another issuer")
	}
	uri, _ := doc["jwks_uri"].(string)
	u, err := neturl.Parse(uri)
	loopback := err == nil && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1" || u.Hostname() == "localhost")
	if err != nil || u.Host == "" || u.User != nil || u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return "", errors.New("auth: the discovery document has no https jwks_uri")
	}
	return uri, nil // egress decides whether http to loopback is allowed (development only)
}

// CheckJWKS fetches and parses a JWKS, returning how many usable signing keys it has.
func CheckJWKS(ctx context.Context, f Fetcher, url string) (int, error) {
	v := &Verifier{Fetch: f}
	keys, _, err := v.fetchJWKS(ctx, url)
	return len(keys), err
}

// Allows reports whether principals hold verb (or admin) on (channel, extension) in a tenant's
// grants: a grant on the tenant, on the channel, or on the extension in every channel or in this one.
func Allows(p Principals, grants []store.Grant, channelID, extension, verb string) bool {
	for _, g := range grants {
		if !p[Key{g.IssuerID, g.Kind, g.Value}] {
			continue
		}
		// admin implies every verb on its resource, except on an issuer-wide grant (spec 0007)
		if !slices.Contains(g.Verbs, verb) && (!slices.Contains(g.Verbs, store.VerbAdmin) || g.Kind == store.PrincipalIssuer) {
			continue
		}
		if g.ChannelID != "" && g.ChannelID != channelID {
			continue
		}
		if g.Extension != "" && g.Extension != extension {
			continue
		}
		return true
	}
	return false
}
