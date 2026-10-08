package tenants

import (
	"context"
	"fmt"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Publishers (spec 0008): identities that only publish and promote, authenticated by credentials.

// AddPublisher adds a publisher to a tenant.
func (s *AuthAdmin) AddPublisher(ctx context.Context, a authz.Actor, tenant, name string) (store.Publisher, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Publisher{}, err
	}
	p := store.Publisher{TenantID: t.ID, Name: name, CreatedBy: a.String()}
	err = s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
	return p, err
}

// ListPublishers lists a tenant's publishers with their credentials.
func (s *AuthAdmin) ListPublishers(ctx context.Context, a authz.Actor, tenant string) ([]store.Publisher, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	ta, err := s.Store.GetTenantAuth(ctx, t.ID)
	return ta.Publishers, err
}

// RemovePublisher removes a publisher, its credentials and its grants.
func (s *AuthAdmin) RemovePublisher(ctx context.Context, a authz.Actor, tenant, name string) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, t.ID, name) })
}

// AddGitHubCredential binds a publisher to GitHub Actions runs of a provider.
func (s *AuthAdmin) AddGitHubCredential(ctx context.Context, a authz.Actor, tenant, publisher string, c store.GitHubCredential) (store.GitHubCredential, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return c, err
	}
	if c.Provider == "" {
		c.Provider = "github"
	}
	known := false
	for _, p := range s.Providers {
		known = known || p.Name == c.Provider
	}
	if !known {
		return c, fmt.Errorf("%w: provider %q is not configured (publish.providers)", store.ErrInvalid, c.Provider)
	}
	if err := auth.CheckRefPattern(c.Ref); err != nil {
		return c, err
	}
	c.CreatedBy = a.String()
	err = s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		p, err := tx.GetPublisher(ctx, t.ID, publisher)
		if err != nil {
			return err
		}
		c.PublisherID = p.ID
		return tx.InsertGitHubCredential(ctx, t.ID, &c)
	})
	return c, err
}

// RemoveGitHubCredential removes a credential of a publisher.
func (s *AuthAdmin) RemoveGitHubCredential(ctx context.Context, a authz.Actor, tenant, publisher, id string) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, "kista/tenant-auth/"+t.ID, func(tx *store.Tx) error {
		p, err := tx.GetPublisher(ctx, t.ID, publisher)
		if err != nil {
			return err
		}
		return tx.DeleteGitHubCredential(ctx, t.ID, p.ID, id)
	})
}
