package auth

import (
	"context"
	"encoding/json"
	"errors"
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
	Claims     map[string]any // the verified token's claims
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
	// LicenseAdmins are the licence managers' principals (spec 0029)
	LicenseAdmins Principals
	// DistributionAdmins are the distribution admins' principals (spec 0030)
	DistributionAdmins Principals
	Verifier           *Verifier
}

// ServerRoles are the server roles a server token's principals hold.
type ServerRoles struct {
	Admin          bool // a server administrator (spec 0007)
	LicenseManager bool // a licence manager (spec 0029): every tenant's licences, nothing else
	// DistributionAdmin: a distribution admin (spec 0030): a distributor's calls on every tenant
	DistributionAdmin bool
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

// Verify verifies a server token and reports the server roles its principals hold.
func (s *Server) Verify(ctx context.Context, token string) (Identity, ServerRoles, error) {
	id, err := s.Verifier.VerifyIdentity(ctx, store.TenantAuth{Issuers: s.Issuers, Audiences: s.Audiences}, "", token)
	if err != nil {
		return id, ServerRoles{}, err
	}
	var r ServerRoles
	for k := range id.Principals {
		r.Admin = r.Admin || s.Admins[k]
		r.LicenseManager = r.LicenseManager || s.LicenseAdmins[k]
		r.DistributionAdmin = r.DistributionAdmin || s.DistributionAdmins[k]
	}
	if r.LicenseManager && r.DistributionAdmin && !r.Admin {
		// one narrow role per token (spec 0030): its principals holding both is a configuration's
		// mistake, refused rather than guessed
		return id, ServerRoles{}, errors.New("auth: the token's principals are both a licence manager and a distribution admin")
	}
	return id, r, nil
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
	if kind == store.PrincipalPublisher { // publisher:<name>: a tenant's publisher (spec 0008)
		if rest == "" || len(rest) > 64 || strings.ContainsAny(rest, "|:/ ") {
			return "", "", "", fmt.Errorf("%w: principal %q is publisher:<name>", store.ErrInvalid, p)
		}
		return kind, "", rest, nil
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
// within 1m..the ceiling in force (limits.ceilings.issuer_token_lifetime), required claims with names
// and values, claim paths.
func CheckIssuer(is *store.Issuer, allowHTTP bool) error {
	return CheckIssuerUnder(is, allowHTTP, store.MaxIssuerTokenLifetime)
}

// CheckIssuerUnder is CheckIssuer with the longest max token lifetime given: the configuration's own
// ceiling as it is loaded, before the store opens and puts it in force.
func CheckIssuerUnder(is *store.Issuer, allowHTTP bool, maxLifetime time.Duration) error {
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
	if is.MaxTokenLifetime < time.Minute || is.MaxTokenLifetime > maxLifetime {
		return fmt.Errorf("%w: max token lifetime must be within 1m..%s (limits.ceilings.issuer_token_lifetime)", store.ErrInvalid,
			maxLifetime)
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
