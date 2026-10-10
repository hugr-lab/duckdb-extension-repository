package tenants

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// canonical is a tenant's canonical audience (spec 0006).
func (s *AuthAdmin) canonical(tenant string) string {
	return strings.TrimSuffix(s.PublicURL, "/") + "/" + tenant
}

// SetConsoleClient sets or replaces the console client of a tenant's issuer record (spec 0015). A
// non-empty expected id must be the record's (store.ErrConflict otherwise): a record re-added under
// the same name never inherits a client meant for the old one. An audience, when set, must be one
// of the tenant's.
func (s *AuthAdmin) SetConsoleClient(ctx context.Context, a authz.Actor, tenant, issuer, expectedID string, c store.ConsoleClient) (store.ConsoleClient, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.ConsoleClient{}, err
	}
	if err := c.Check(); err != nil {
		return store.ConsoleClient{}, err
	}
	c.CreatedBy = a.String()
	err = s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		rec, err := tx.GetIssuer(ctx, t.ID, issuer)
		if err != nil {
			return err
		}
		if expectedID != "" && rec.ID != expectedID {
			return store.ErrConflict
		}
		if slices.Contains(s.ServerIssuerURLs, rec.URL) {
			return fmt.Errorf("%w: %s is a server issuer: a tenant's console does not sign in there", store.ErrInvalid, rec.URL)
		}
		if c.Audience != "" && c.Audience != s.canonical(t.Name) {
			auds, err := tx.TenantAudiences(ctx, t.ID)
			if err != nil {
				return err
			}
			if !slices.Contains(auds, c.Audience) {
				return fmt.Errorf("%w: %s is not one of the tenant's audiences", store.ErrInvalid, c.Audience)
			}
		}
		c.IssuerID = rec.ID
		if err := tx.SetConsoleClient(ctx, &c); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "issuer.console.set", "issuer:"+rec.Name,
			map[string]any{"issuer": rec.Name, "client_id": c.ClientID, "audience": c.Audience})
	})
	return c, err
}

// RemoveConsoleClient removes the console client of a tenant's issuer record (spec 0015), with the
// same expected id rule as SetConsoleClient.
func (s *AuthAdmin) RemoveConsoleClient(ctx context.Context, a authz.Actor, tenant, issuer, expectedID string) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		rec, err := tx.GetIssuer(ctx, t.ID, issuer)
		if err != nil {
			return err
		}
		if expectedID != "" && rec.ID != expectedID {
			return store.ErrConflict
		}
		old, err := tx.GetConsoleClient(ctx, rec.ID)
		if err != nil {
			return err
		}
		if err := tx.RemoveConsoleClient(ctx, rec.ID); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "issuer.console.remove", "issuer:"+rec.Name,
			map[string]any{"issuer": rec.Name, "client_id": old.ClientID, "audience": old.Audience})
	})
}

// ConsoleIssuer is an issuer a tenant's console signs in with (spec 0015): public.
type ConsoleIssuer struct {
	Name              string   `json:"name"`
	Issuer            string   `json:"issuer"`
	ClientID          string   `json:"client_id"`
	Scopes            []string `json:"scopes"`
	AudienceParameter string   `json:"audience_parameter"`
	Audience          string   `json:"audience"`
}

// ConsoleIssuers lists the issuers a tenant's console signs in with: the records with a console
// client, each with the audience its tokens carry. It authorizes nothing: the route is public, and
// the caller answers 404 for an unknown or suspended tenant before calling it.
func (s *AuthAdmin) ConsoleIssuers(ctx context.Context, t store.Tenant) ([]ConsoleIssuer, error) {
	iss, ccs, err := s.Store.ConsoleClients(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	out := make([]ConsoleIssuer, 0, len(iss))
	for i, is := range iss {
		c := ccs[i]
		aud := c.Audience
		if aud == "" {
			aud = s.canonical(t.Name)
		}
		out = append(out, ConsoleIssuer{Name: is.Name, Issuer: is.URL, ClientID: c.ClientID, Scopes: c.Scopes,
			AudienceParameter: c.AudienceParameter, Audience: aud})
	}
	return out, nil
}
