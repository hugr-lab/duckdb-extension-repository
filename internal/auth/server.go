package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Identity is a verified token: its principals and what an actor is recorded as.
type Identity struct {
	Principals Principals
	Issuer     store.Issuer // the record (or server issuer) that verified it
	Subject    string       // sub, if clean
	Client     string       // the record's client claim, if any
	IssuedAt   time.Time
}

// Who is who the token names, as recorded after its issuer: the sub, or client:<client id> without
// one; empty when it names neither (spec 0007: writers are named).
func (id Identity) Who() string {
	switch {
	case strings.HasPrefix(id.Subject, "client:") || strings.HasPrefix(id.Subject, "sub:"):
		return "sub:" + id.Subject // never read as a client id
	case id.Subject != "":
		return id.Subject
	case id.Client != "":
		return "client:" + id.Client
	}
	return ""
}

// Audiences reads a token's aud claim without verifying it: only to choose which issuers verify it
// (spec 0007: a token carrying a server audience is a server token, whatever else it carries).
func Audiences(token string) []string {
	t, err := parse(token)
	if err != nil {
		return nil
	}
	switch x := t.claims["aud"].(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ServerIssuerID is the issuer id of a server issuer's principals: no tenant record has one (tenant
// record ids are UUIDs), so server principals never match a tenant grant and the other way round.
func ServerIssuerID(name string) string { return "server:" + name }

// Server verifies server tokens (spec 0007): the server issuers from config, with a verifier (and
// so a JWKS cache and an egress fetcher) of their own.
type Server struct {
	Issuers   []store.Issuer
	Audiences []string
	Admins    Principals
	Verifier  *Verifier
}

// IsServerToken reports whether a token carries a server audience.
func (s *Server) IsServerToken(token string) bool {
	if s == nil {
		return false
	}
	for _, a := range Audiences(token) {
		if slices.Contains(s.Audiences, a) {
			return true
		}
	}
	return false
}

// Verify verifies a server token and reports whether it is a server administrator's.
func (s *Server) Verify(ctx context.Context, token string) (Identity, bool, error) {
	id, err := s.Verifier.VerifyIdentity(ctx, store.TenantAuth{Issuers: s.Issuers, Audiences: s.Audiences}, "", token)
	if err != nil {
		return id, false, err
	}
	for k := range id.Principals {
		if s.Admins[k] {
			return id, true, nil
		}
	}
	return id, false, nil
}

// ParsePrincipalKey parses a principal ("subject:<issuer>|<value>", role, group, client alike; or
// "issuer:<issuer>") to its kind, issuer name and value.
func ParsePrincipalKey(p string) (kind, issuer, value string, err error) {
	kind, rest, ok := strings.Cut(p, ":")
	if !ok {
		return "", "", "", fmt.Errorf("%w: principal %q is kind:issuer|value or issuer:name", store.ErrInvalid, p)
	}
	if kind == store.PrincipalIssuer {
		return kind, rest, "", store.ValidIssuerName(rest)
	}
	if !slices.Contains([]string{store.PrincipalSubject, store.PrincipalRole, store.PrincipalGroup, store.PrincipalClient}, kind) {
		return "", "", "", fmt.Errorf("%w: principal kind %q", store.ErrInvalid, kind)
	}
	issuer, value, ok = strings.Cut(rest, "|")
	if !ok || value == "" || !cleanValue(value) {
		return "", "", "", fmt.Errorf("%w: principal %q is kind:issuer|value", store.ErrInvalid, p)
	}
	return kind, issuer, value, store.ValidIssuerName(issuer)
}

// ParseClaimPath parses a claim path: a JSON array of keys (for keys with dots), or keys joined by
// dots.
func ParseClaimPath(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var p []string
	if strings.HasPrefix(s, "[") {
		if err := json.Unmarshal([]byte(s), &p); err != nil || len(p) == 0 {
			return nil, fmt.Errorf("%w: claim path %q", store.ErrInvalid, s)
		}
	} else {
		p = strings.Split(s, ".")
	}
	return p, CheckClaimPath(p)
}

// CheckClaimPath checks a claim path's keys.
func CheckClaimPath(p []string) error {
	if len(p) > 8 {
		return fmt.Errorf("%w: a claim path has at most 8 keys", store.ErrInvalid)
	}
	for _, k := range p {
		if k == "" || len(k) > 128 {
			return fmt.Errorf("%w: a claim path has an empty or long key", store.ErrInvalid)
		}
	}
	return nil
}

// CheckIssuer checks an issuer record's fields and fills the defaults (every algorithm, a 24h
// maximum lifetime): https URLs (http on loopback when allowHTTP), known algorithms, a lifetime
// within 1m..7d, required claims with names and values, claim paths.
func CheckIssuer(is *store.Issuer, allowHTTP bool) error {
	if err := store.ValidIssuerName(is.Name); err != nil {
		return err
	}
	if err := checkURL(is.URL, "issuer URL", allowHTTP); err != nil {
		return err
	}
	if is.JWKSURI != "" {
		if err := checkURL(is.JWKSURI, "jwks_uri", allowHTTP); err != nil {
			return err
		}
	}
	if len(is.Algorithms) == 0 {
		is.Algorithms = Algorithms
	}
	var algs []string
	for _, alg := range is.Algorithms {
		if !slices.Contains(Algorithms, alg) {
			return fmt.Errorf("%w: algorithm %q (one of %s)", store.ErrInvalid, alg, strings.Join(Algorithms, ", "))
		}
		if !slices.Contains(algs, alg) {
			algs = append(algs, alg)
		}
	}
	is.Algorithms = algs
	if is.MaxTokenLifetime == 0 {
		is.MaxTokenLifetime = 24 * time.Hour
	}
	if is.MaxTokenLifetime < time.Minute || is.MaxTokenLifetime > 7*24*time.Hour {
		return fmt.Errorf("%w: max token lifetime must be within 1m..7d", store.ErrInvalid)
	}
	if len(is.RequiredClaims) > 16 {
		return fmt.Errorf("%w: at most 16 required claims", store.ErrInvalid)
	}
	for k, v := range is.RequiredClaims {
		if k == "" || v == "" || len(k) > 128 || len(v) > maxClaimLen {
			return fmt.Errorf("%w: a required claim needs a name and a value", store.ErrInvalid)
		}
	}
	for _, p := range [][]string{is.RolesClaim, is.GroupsClaim, is.ClientClaim} {
		if err := CheckClaimPath(p); err != nil {
			return err
		}
	}
	return nil
}

func checkURL(raw, what string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(raw) > 512 {
		return fmt.Errorf("%w: %s %q", store.ErrInvalid, what, raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && allowHTTP && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		return nil
	}
	return fmt.Errorf("%w: %s must be https", store.ErrInvalid, what)
}
