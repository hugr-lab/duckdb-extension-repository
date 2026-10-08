package tenants

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// AuthAdmin manages a tenant's issuer records, audiences and grants (spec 0006). Server-wide
// actions (audiences) need the server administrator.
type AuthAdmin struct {
	Store     *store.Store
	Authz     authz.Authorizer
	Fetch     auth.Fetcher // egress, for discovery at issuer add
	PublicURL string       // audiences under it are canonical and cannot be assigned
	AllowHTTP bool         // issuer URLs on loopback over http (profile dev)
}

func (s *AuthAdmin) tenant(ctx context.Context, a authz.Actor, verb authz.Verb, name string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, verb, name, ""); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, name)
}

// AddIssuer adds an issuer record. Without an explicit JWKS URI, discovery runs now (through
// egress) and must succeed; it is repeated in the background when serving.
func (s *AuthAdmin) AddIssuer(ctx context.Context, a authz.Actor, tenant string, is store.Issuer) (store.Issuer, error) {
	t, err := s.tenant(ctx, a, authz.VerbAdmin, tenant)
	if err != nil {
		return store.Issuer{}, err
	}
	if err := s.checkURL(is.URL, "issuer URL"); err != nil {
		return store.Issuer{}, err
	}
	if is.JWKSURI != "" {
		if err := s.checkURL(is.JWKSURI, "jwks_uri"); err != nil {
			return store.Issuer{}, err
		}
	}
	if len(is.Algorithms) == 0 {
		is.Algorithms = auth.Algorithms
	}
	for _, alg := range is.Algorithms {
		if !slices.Contains(auth.Algorithms, alg) {
			return store.Issuer{}, fmt.Errorf("%w: algorithm %q (one of %s)", store.ErrInvalid, alg, strings.Join(auth.Algorithms, ", "))
		}
	}
	if is.MaxTokenLifetime == 0 {
		is.MaxTokenLifetime = 24 * time.Hour
	}
	if is.MaxTokenLifetime < time.Minute || is.MaxTokenLifetime > 7*24*time.Hour {
		return store.Issuer{}, fmt.Errorf("%w: max token lifetime must be within 1m..7d", store.ErrInvalid)
	}
	for k, v := range is.RequiredClaims {
		if k == "" || v == "" {
			return store.Issuer{}, fmt.Errorf("%w: a required claim needs a name and a value", store.ErrInvalid)
		}
	}
	jwksURI := is.JWKSURI
	if jwksURI == "" {
		if jwksURI, err = auth.Discover(ctx, s.Fetch, is.URL); err != nil {
			return store.Issuer{}, fmt.Errorf("discovery for %s: %w", is.URL, err)
		}
	}
	if n, err := auth.CheckJWKS(ctx, s.Fetch, jwksURI); err != nil {
		return store.Issuer{}, fmt.Errorf("the JWKS of %s: %w", is.URL, err)
	} else if n == 0 {
		return store.Issuer{}, fmt.Errorf("the JWKS of %s has no usable signing key", is.URL)
	}
	is.Algorithms = dedupe(is.Algorithms)
	is.TenantID, is.CreatedBy = t.ID, a.String()
	err = s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error { return tx.InsertIssuer(ctx, &is) })
	return is, err
}

func (s *AuthAdmin) checkURL(raw, what string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(raw) > 512 {
		return fmt.Errorf("%w: %s %q", store.ErrInvalid, what, raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && s.AllowHTTP && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		return nil
	}
	return fmt.Errorf("%w: %s must be https", store.ErrInvalid, what)
}

// ListIssuers lists a tenant's issuer records.
func (s *AuthAdmin) ListIssuers(ctx context.Context, a authz.Actor, tenant string) ([]store.Issuer, error) {
	t, err := s.tenant(ctx, a, authz.VerbRead, tenant)
	if err != nil {
		return nil, err
	}
	ta, err := s.Store.GetTenantAuth(ctx, t.ID)
	return ta.Issuers, err
}

// RemoveIssuer removes an issuer record and its grants.
func (s *AuthAdmin) RemoveIssuer(ctx context.Context, a authz.Actor, tenant, name string) error {
	t, err := s.tenant(ctx, a, authz.VerbAdmin, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error { return tx.DeleteIssuer(ctx, t.ID, name) })
}

// AddAudience assigns an audience to a tenant (server-wide action: a tenant administrator cannot
// choose one, or could claim another tenant's).
func (s *AuthAdmin) AddAudience(ctx context.Context, a authz.Actor, tenant, aud string) error {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, "", ""); err != nil {
		return err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return err
	}
	p := strings.TrimSuffix(s.PublicURL, "/")
	if p == "" {
		return fmt.Errorf("%w: serve.public_url must be set to assign audiences (the canonical ones are under it)", store.ErrInvalid)
	}
	if aud == p || strings.HasPrefix(aud, p+"/") {
		return fmt.Errorf("%w: audiences under %s are the canonical ones (and %s itself is the server's)", store.ErrInvalid, p, p)
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error { return tx.AddAudience(ctx, t.ID, aud, a.String()) })
}

// RemoveAudience removes a tenant's assigned audience (server-wide action).
func (s *AuthAdmin) RemoveAudience(ctx context.Context, a authz.Actor, tenant, aud string) error {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, "", ""); err != nil {
		return err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error { return tx.RemoveAudience(ctx, t.ID, aud) })
}

// ListAudiences lists a tenant's audiences: the canonical one first, then the assigned ones.
func (s *AuthAdmin) ListAudiences(ctx context.Context, a authz.Actor, tenant string) ([]string, error) {
	t, err := s.tenant(ctx, a, authz.VerbRead, tenant)
	if err != nil {
		return nil, err
	}
	ta, err := s.Store.GetTenantAuth(ctx, t.ID)
	var out []string
	if s.PublicURL != "" {
		out = append(out, strings.TrimSuffix(s.PublicURL, "/")+"/"+t.Name)
	}
	return append(out, ta.Audiences...), err
}

// ParsePrincipal parses "subject:<issuer>|<value>" (role, group, client alike) or "issuer:<issuer>".
func ParsePrincipal(p string) (kind, issuer, value string, err error) {
	kind, rest, ok := strings.Cut(p, ":")
	if !ok {
		return "", "", "", fmt.Errorf("%w: principal %q is kind:issuer|value or issuer:name", store.ErrInvalid, p)
	}
	if kind == store.PrincipalIssuer {
		return kind, rest, "", store.ValidIssuerName(rest)
	}
	issuer, value, ok = strings.Cut(rest, "|")
	if !ok || value == "" {
		return "", "", "", fmt.Errorf("%w: principal %q is kind:issuer|value", store.ErrInvalid, p)
	}
	return kind, issuer, value, store.ValidIssuerName(issuer)
}

// AddGrant grants verbs to a principal on the tenant, a channel, or an extension (in every channel
// or one).
func (s *AuthAdmin) AddGrant(ctx context.Context, a authz.Actor, tenant, principal string, verbs []string, channel, extension string) (store.Grant, error) {
	t, err := s.tenant(ctx, a, authz.VerbAdmin, tenant)
	if err != nil {
		return store.Grant{}, err
	}
	kind, issuer, value, err := ParsePrincipal(principal)
	if err != nil {
		return store.Grant{}, err
	}
	if extension != "" {
		if err := release.ValidName(extension); err != nil {
			return store.Grant{}, err
		}
	}
	g := store.Grant{TenantID: t.ID, Kind: kind, Value: value, Extension: extension, Verbs: verbs, CreatedBy: a.String()}
	g.Verbs = dedupe(g.Verbs)
	err = s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		rec, err := tx.GetIssuer(ctx, t.ID, issuer)
		if err != nil {
			return err
		}
		if kind == store.PrincipalIssuer && len(rec.RequiredClaims) == 0 {
			return fmt.Errorf("%w: an issuer: grant needs the record to have required claims (any account of a shared issuer could use it)", store.ErrInvalid)
		}
		g.IssuerID, g.IssuerName = rec.ID, rec.Name
		if channel != "" {
			c, err := tx.GetChannel(ctx, tenant, channel)
			if err != nil {
				return err
			}
			g.ChannelID, g.ChannelName = c.ID, c.Name
		}
		return tx.InsertGrant(ctx, &g)
	})
	return g, err
}

func dedupe(in []string) []string {
	var out []string
	for _, v := range in {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// ListGrants lists a tenant's grants.
func (s *AuthAdmin) ListGrants(ctx context.Context, a authz.Actor, tenant string) ([]store.Grant, error) {
	t, err := s.tenant(ctx, a, authz.VerbRead, tenant)
	if err != nil {
		return nil, err
	}
	ta, err := s.Store.GetTenantAuth(ctx, t.ID)
	return ta.Grants, err
}

// RemoveGrant removes a grant.
func (s *AuthAdmin) RemoveGrant(ctx context.Context, a authz.Actor, tenant, id string) error {
	t, err := s.tenant(ctx, a, authz.VerbAdmin, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error { return tx.DeleteGrant(ctx, t.ID, id) })
}
