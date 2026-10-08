package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Provider is a trusted-publishing token issuer (spec 0008): GitHub Actions, or a GitHub Enterprise
// Server, with a verifier of its own.
type Provider struct {
	Name, URL string
	Verifier  *Verifier
}

// Providers are the configured providers.
type Providers []Provider

// For returns the provider whose URL is a token's issuer, exactly.
func (ps Providers) For(iss string) *Provider {
	for i := range ps {
		if ps[i].URL == iss {
			return &ps[i]
		}
	}
	return nil
}

// Has reports whether a URL is a provider's: a tenant issuer record cannot have it.
func (ps Providers) Has(url string) bool { return ps.For(url) != nil }

// Issuer reads a token's iss claim without verifying it: only to choose who verifies it.
func Issuer(token string) string {
	t, err := parse(token)
	if err != nil {
		return ""
	}
	iss, _ := t.claims["iss"].(string)
	return iss
}

// providerLifetime bounds a provider token's lifetime (GitHub's are minutes).
const providerLifetime = time.Hour

// refusedEvents run code a contributor controls with the base repository's identity.
var refusedEvents = []string{"pull_request", "pull_request_target", "workflow_run", "merge_group"}

// Publication is a verified provider token: the publishers whose credentials match, as principals,
// and the run's facts for provenance.
type Publication struct {
	Identity   Identity
	Publishers []MatchedPublisher
	Provider   string
}

// MatchedPublisher is a publisher and the credential that matched.
type MatchedPublisher struct {
	Publisher  store.Publisher
	Credential string // github:<credential id>, or key:<prefix>
}

// PublisherKey is a publisher's principal.
func PublisherKey(publisherID string) Key {
	return Key{Kind: store.PrincipalPublisher, Value: publisherID}
}

// Verify verifies a provider token for a tenant: the canonical audience only, a lifetime of at most
// an hour, no pull-request events; the tenant's credentials of this provider whose fields all match
// make their publishers the principals. A token that matches none is not valid here.
func (p *Provider) Verify(ctx context.Context, ta store.TenantAuth, canonical, token string) (Publication, error) {
	rec := store.Issuer{ID: "provider:" + p.Name, Name: p.Name, URL: p.URL, Algorithms: []string{"RS256"},
		MaxTokenLifetime: providerLifetime}
	id, err := p.Verifier.VerifyIdentity(ctx, store.TenantAuth{Issuers: []store.Issuer{rec}}, canonical, token)
	if err != nil {
		return Publication{}, err
	}
	c := id.Claims
	str := func(k string) string { s, _ := c[k].(string); return s }
	for _, ev := range refusedEvents {
		if str("event_name") == ev {
			return Publication{}, &Failure{Reason: "event " + ev, Issuer: p.Name}
		}
	}
	pub := Publication{Identity: id, Provider: p.Name}
	for _, pb := range ta.Publishers {
		for _, cr := range pb.GitHub {
			// the calling workflow at the run's ref, exactly (a file name may contain "@")
			if cr.Provider != p.Name || str("repository_owner_id") != cr.OwnerID || str("repository_id") != cr.RepositoryID ||
				str("ref") == "" || str("workflow_ref") != cr.Workflow+"@"+str("ref") ||
				cr.Environment != "" && str("environment") != cr.Environment ||
				cr.Ref != "" && !RefMatch(cr.Ref, str("ref")) {
				continue
			}
			pub.Publishers = append(pub.Publishers, MatchedPublisher{Publisher: pb, Credential: "github:" + cr.ID})
			break
		}
	}
	if len(pub.Publishers) == 0 {
		return Publication{}, &Failure{Reason: "no publisher credential matches", Issuer: p.Name}
	}
	pub.Identity.Principals = Principals{}
	for _, m := range pub.Publishers {
		pub.Identity.Principals[PublisherKey(m.Publisher.ID)] = true
	}
	return pub, nil
}

// Provenance is a publication's record (spec 0008): the actor, the provider and the run.
func (p Publication) Provenance(actor string) string {
	c := p.Identity.Claims
	m := map[string]any{"actor": actor, "provider": p.Provider}
	for _, k := range []string{"repository", "repository_id", "repository_owner", "repository_owner_id", "workflow_ref", "ref",
		"sha", "run_id", "run_attempt", "environment"} {
		if s, ok := c[k].(string); ok && cleanValue(s) {
			m[k] = s
		}
	}
	creds := []string{}
	for _, x := range p.Publishers {
		creds = append(creds, x.Credential)
	}
	m["credentials"] = creds
	b, err := json.Marshal(m)
	if err != nil || len(b) > 4000 {
		b, _ = json.Marshal(map[string]any{"actor": actor, "provider": p.Provider})
	}
	return string(b)
}

// maxRef bounds the refs a pattern is matched against.
const maxRef = 1024

// RefMatch matches a git ref against a pattern: "*" within a path segment, "**" across segments.
// The match is a table over (pattern segment, ref segment), each cell a linear wildcard match, so a
// hostile ref costs at most a quadratic amount of work.
func RefMatch(pattern, ref string) bool {
	if len(ref) > maxRef {
		return false
	}
	ps, rs := strings.Split(pattern, "/"), strings.Split(ref, "/")
	// ok[i][j]: ps[i:] matches rs[j:]
	ok := make([][]bool, len(ps)+1)
	for i := range ok {
		ok[i] = make([]bool, len(rs)+1)
	}
	ok[len(ps)][len(rs)] = true
	for i := len(ps) - 1; i >= 0; i-- {
		for j := len(rs); j >= 0; j-- {
			if ps[i] == "**" {
				ok[i][j] = ok[i+1][j] || j < len(rs) && ok[i][j+1]
			} else {
				ok[i][j] = j < len(rs) && ok[i+1][j+1] && segment(ps[i], rs[j])
			}
		}
	}
	return ok[0][0]
}

// segment matches one path segment against a pattern where "*" is any run of characters: the
// greedy wildcard match, backtracking only to the last star (linear in practice).
func segment(p, s string) bool {
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && p[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			pi, mark = star+1, mark+1
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// CheckRefPattern checks a credential's ref pattern.
func CheckRefPattern(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > 256 || strings.Count(p, "*") > 8 || !strings.HasPrefix(p, "refs/") {
		return fmt.Errorf("%w: ref is a pattern over refs/... with at most 8 stars", store.ErrInvalid)
	}
	return nil
}
