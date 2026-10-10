package tenants

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

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
	// ServerAudiences are the server tokens' audiences (spec 0007): no tenant may be assigned one.
	ServerAudiences []string
	// Providers are the trusted-publishing issuers (spec 0008): no issuer record may have their URLs.
	Providers auth.Providers
	// ServerIssuerURLs are the server issuers' URLs (spec 0007): a tenant's console may not sign in
	// there, or a tenant could lead a server administrator to act inside it unawares (spec 0015).
	ServerIssuerURLs []string
}

// ErrIssuerFetch is an issuer whose discovery document or JWKS could not be fetched or used when it
// was added.
var ErrIssuerFetch = errors.New("tenants: the issuer's discovery document or JWKS could not be fetched or used")

// ErrLastAdmin refuses a change by a tenant principal that would leave its tenant without a
// tenant-wide admin grant (spec 0007: no lock-out by accident).
var ErrLastAdmin = errors.New("tenants: the tenant's last tenant-wide admin grant; a server administrator can remove it")

func (s *AuthAdmin) tenant(ctx context.Context, a authz.Actor, name string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: name}); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, name)
}

// keepAdmin runs inside a removal's transaction (under the tenant's auth lock): a tenant principal
// may not remove the last tenant-wide admin grant. It counts grants, not people.
func keepAdmin(ctx context.Context, tx *store.Tx, a authz.Actor, tenantID string, before int) error {
	if a.Kind != authz.ActorPrincipal || before == 0 {
		return nil
	}
	n, err := tx.TenantAdminGrants(ctx, tenantID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

// AddIssuer adds an issuer record. Without an explicit JWKS URI, discovery runs now (through
// egress) and must succeed; it is repeated in the background when serving.
func (s *AuthAdmin) AddIssuer(ctx context.Context, a authz.Actor, tenant string, is store.Issuer) (store.Issuer, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Issuer{}, err
	}
	if err := auth.CheckIssuer(&is, s.AllowHTTP); err != nil {
		return store.Issuer{}, err
	}
	if s.Providers.Has(is.URL) {
		return store.Issuer{}, fmt.Errorf("%w: %s is a trusted-publishing provider: add a publisher with its credential instead", store.ErrInvalid, is.URL)
	}
	jwksURI := is.JWKSURI
	if jwksURI == "" {
		if jwksURI, err = auth.Discover(ctx, s.Fetch, is.URL); err != nil {
			return store.Issuer{}, fmt.Errorf("%w: discovery for %s: %w", ErrIssuerFetch, is.URL, err)
		}
	}
	if n, err := auth.CheckJWKS(ctx, s.Fetch, jwksURI); err != nil {
		return store.Issuer{}, fmt.Errorf("%w: the JWKS of %s: %w", ErrIssuerFetch, is.URL, err)
	} else if n == 0 {
		return store.Issuer{}, fmt.Errorf("%w: the JWKS of %s has no usable signing key", ErrIssuerFetch, is.URL)
	}
	is.TenantID, is.CreatedBy = t.ID, a.String()
	err = s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		if err := tx.InsertIssuer(ctx, &is); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "issuer.add", "issuer:"+is.Name,
			map[string]any{"name": is.Name, "url": is.URL, "algorithms": is.Algorithms})
	})
	return is, err
}

// ListIssuers lists a tenant's issuer records.
func (s *AuthAdmin) ListIssuers(ctx context.Context, a authz.Actor, tenant string) ([]store.Issuer, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	ta, err := s.Store.GetTenantAuth(ctx, t.ID)
	return ta.Issuers, err
}

// RemoveIssuer removes an issuer record and its grants. A non-empty expected id must be the
// record's (store.ErrConflict otherwise): a record re-added under the same name is not removed by
// mistake.
func (s *AuthAdmin) RemoveIssuer(ctx context.Context, a authz.Actor, tenant, name, expectedID string) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		rec, err := tx.GetIssuer(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if expectedID != "" && rec.ID != expectedID {
			return store.ErrConflict
		}
		before, err := tx.TenantAdminGrants(ctx, t.ID)
		if err != nil {
			return err
		}
		if err := tx.DeleteIssuer(ctx, t.ID, name); err != nil {
			return err
		}
		if err := tx.Event(ctx, t.ID, a.String(), "issuer.remove", "issuer:"+rec.Name, map[string]any{"name": rec.Name, "url": rec.URL}); err != nil {
			return err
		}
		return keepAdmin(ctx, tx, a, t.ID, before)
	})
}

// AddAudience assigns an audience to a tenant (server-wide action: a tenant administrator cannot
// choose one, or could claim another tenant's).
func (s *AuthAdmin) AddAudience(ctx context.Context, a authz.Actor, tenant, aud string) error {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
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
	if slices.Contains(s.ServerAudiences, aud) {
		return fmt.Errorf("%w: %s is a server audience", store.ErrInvalid, aud)
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		if err := tx.AddAudience(ctx, t.ID, aud, a.String()); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "audience.add", "tenant:"+t.Name, map[string]any{"audience": aud})
	})
}

// RemoveAudience removes a tenant's assigned audience (server-wide action).
func (s *AuthAdmin) RemoveAudience(ctx context.Context, a authz.Actor, tenant, aud string) error {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		// a console client signing in for it would advertise an audience the tenant no longer has
		if used, err := tx.ConsoleClientUses(ctx, t.ID, aud); err != nil || used {
			if err == nil {
				err = fmt.Errorf("%w: an issuer's console client asks for %s: change it first", store.ErrInvalid, aud)
			}
			return err
		}
		if err := tx.RemoveAudience(ctx, t.ID, aud); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "audience.remove", "tenant:"+t.Name, map[string]any{"audience": aud})
	})
}

// ListAudiences lists a tenant's audiences: the canonical one first, then the assigned ones.
func (s *AuthAdmin) ListAudiences(ctx context.Context, a authz.Actor, tenant string) ([]string, error) {
	t, err := s.tenant(ctx, a, tenant)
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
	return auth.ParsePrincipalKey(p)
}

// AddGrant grants verbs to a principal on the tenant, a channel, or an extension (in every channel
// or one). An issuer-wide grant cannot carry admin: every account of the issuer would hold it.
func (s *AuthAdmin) AddGrant(ctx context.Context, a authz.Actor, tenant, principal string, verbs []string, channel, extension string) (store.Grant, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Grant{}, err
	}
	kind, issuer, value, err := ParsePrincipal(principal)
	if err != nil {
		return store.Grant{}, err
	}
	if kind == store.PrincipalIssuer && (slices.Contains(verbs, store.VerbAdmin) || slices.Contains(verbs, store.VerbPublish) ||
		slices.Contains(verbs, store.VerbPromote) || slices.Contains(verbs, store.VerbAudit)) {
		return store.Grant{}, fmt.Errorf("%w: an issuer: grant cannot carry admin, publish, promote or audit", store.ErrInvalid)
	}
	if extension != "" {
		if err := release.ValidName(extension); err != nil {
			return store.Grant{}, err
		}
	}
	g := store.Grant{TenantID: t.ID, Kind: kind, Value: value, Extension: extension, Verbs: verbs, CreatedBy: a.String()}
	if kind == store.PrincipalPublisher {
		g.Value = ""
	}
	g.Verbs = dedupe(g.Verbs)
	err = s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		if kind == store.PrincipalPublisher {
			p, err := tx.GetPublisher(ctx, t.ID, value)
			if err != nil {
				return err
			}
			g.PublisherID, g.PublisherName = p.ID, p.Name
		} else {
			rec, err := tx.GetIssuer(ctx, t.ID, issuer)
			if err != nil {
				return err
			}
			if kind == store.PrincipalIssuer && len(rec.RequiredClaims) == 0 {
				return fmt.Errorf("%w: an issuer: grant needs the record to have required claims (any account of a shared issuer could use it)", store.ErrInvalid)
			}
			g.IssuerID, g.IssuerName = rec.ID, rec.Name
		}
		if channel != "" {
			c, err := tx.GetChannel(ctx, tenant, channel)
			if err != nil {
				return err
			}
			g.ChannelID, g.ChannelName = c.ID, c.Name
		}
		if err := tx.InsertGrant(ctx, &g); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "grant.add", "grant:"+g.ID, grantFields(g))
	})
	return g, err
}

func grantFields(g store.Grant) map[string]any {
	return map[string]any{"principal": g.Principal(), "verbs": g.Verbs, "channel": g.ChannelName, "extension": g.Extension}
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
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	ta, err := s.Store.GetTenantAuth(ctx, t.ID)
	return ta.Grants, err
}

// RemoveGrant removes a grant.
func (s *AuthAdmin) RemoveGrant(ctx context.Context, a authz.Actor, tenant, id string) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	ta, err := s.Store.GetTenantAuth(ctx, t.ID) // the grant's fields for its event (grants never change)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	for _, g := range ta.Grants {
		if g.ID == id {
			fields = grantFields(g)
		}
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		before, err := tx.TenantAdminGrants(ctx, t.ID)
		if err != nil {
			return err
		}
		if err := tx.DeleteGrant(ctx, t.ID, id); err != nil {
			return err
		}
		if err := tx.Event(ctx, t.ID, a.String(), "grant.remove", "grant:"+id, fields); err != nil {
			return err
		}
		return keepAdmin(ctx, tx, a, t.ID, before)
	})
}
