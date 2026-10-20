package tenants

import (
	"context"
	"fmt"
	"time"

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
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		if err := tx.InsertPublisher(ctx, &p); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "publisher.add", "publisher:"+p.Name, map[string]any{"name": p.Name})
	})
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
	return s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		if err := tx.DeletePublisher(ctx, t.ID, name); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "publisher.remove", "publisher:"+name, map[string]any{"name": name})
	})
}

// SetPublisherLicenseKeys marks a publisher a trusted source of licence keys, or unmarks it (spec
// 0021): a server administrator's act. While marked, its credentials and grants are the server
// administrator's to change; unmarking keeps the keys it brought.
func (s *AuthAdmin) SetPublisherLicenseKeys(ctx context.Context, a authz.Actor, tenant, name string, on bool) (store.Publisher, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.Publisher{}, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Publisher{}, err
	}
	var p store.Publisher
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		if on {
			// credentials a tenant administrator made before the mark would outlive it: the server
			// administrator adds them once it is marked
			cur, err := tx.GetPublisher(ctx, t.ID, name)
			if err != nil {
				return err
			}
			if n, err := tx.PublisherCredentials(ctx, cur.ID); err != nil {
				return err
			} else if n > 0 && !cur.LicenseKeys {
				return fmt.Errorf("%w: publisher %s holds keys (%d); remove them, mark it, then add them "+
					"again as a server administrator", store.ErrInvalid, name, n)
			}
		}
		if p, err = tx.SetPublisherLicenseKeys(ctx, t.ID, name, on); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "publisher.change", "publisher:"+p.Name,
			map[string]any{"name": p.Name, "license_keys": on})
	})
	return p, err
}

// marked refuses a change to a marked publisher's credentials or grants unless a server
// administrator makes it (spec 0021): a tenant administrator cannot point it at builds of their own.
func (s *AuthAdmin) marked(ctx context.Context, a authz.Actor, p store.Publisher) error {
	if !p.LicenseKeys {
		return nil
	}
	return s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server)
}

// MaxKeyLifetime is the furthest a key's expiry may be.
const MaxKeyLifetime = 366 * 24 * time.Hour

// AddPublisherKey registers a public SSH key on a publisher (spec 0023): an OpenSSH line, Ed25519
// or ECDSA P-256, unique in the tenant; expires is optional (zero: none), within a year.
func (s *AuthAdmin) AddPublisherKey(ctx context.Context, a authz.Actor, tenant, publisher, line string, expires time.Time) (store.PublisherKey, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.PublisherKey{}, err
	}
	pk, err := auth.ParsePublicKey(line)
	if err != nil {
		return store.PublisherKey{}, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	if now := s.Store.Now(); !expires.IsZero() && (!expires.After(now) || expires.Sub(now) > MaxKeyLifetime) {
		return store.PublisherKey{}, fmt.Errorf("%w: a key expires within a year, or never", store.ErrInvalid)
	}
	k := store.PublisherKey{TenantID: t.ID, Type: pk.Type, PublicKey: auth.AuthorizedKey(pk.Wire), Fingerprint: pk.Fingerprint,
		Title: pk.Title, ExpiresAt: expires, CreatedBy: a.String()}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		p, err := tx.GetPublisher(ctx, t.ID, publisher)
		if err != nil {
			return err
		}
		if err := s.marked(ctx, a, p); err != nil {
			return err
		}
		k.PublisherID = p.ID
		if err := tx.InsertPublisherKey(ctx, &k); err != nil {
			return err
		}
		f := map[string]any{"publisher": p.Name, "key": k.ID, "fingerprint": k.Fingerprint, "type": k.Type, "title": k.Title}
		if !expires.IsZero() {
			f["expires_at"] = expires
		}
		return tx.Event(ctx, t.ID, a.String(), "publisher.key.add", "publisher:"+p.Name+"/key:"+k.ID, f)
	})
	if err != nil {
		return store.PublisherKey{}, err
	}
	return k, nil
}

// ListPublisherKeys lists a publisher's keys.
func (s *AuthAdmin) ListPublisherKeys(ctx context.Context, a authz.Actor, tenant, publisher string) ([]store.PublisherKey, error) {
	p, err := s.publisher(ctx, a, tenant, publisher)
	if err != nil {
		return nil, err
	}
	return s.Store.PublisherKeys(ctx, p.TenantID, p.ID)
}

// TenantPublisherKeys lists every key of a tenant's publishers, by publisher id.
func (s *AuthAdmin) TenantPublisherKeys(ctx context.Context, a authz.Actor, tenant string) (map[string][]store.PublisherKey, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	ks, err := s.Store.PublisherKeys(ctx, t.ID, "")
	if err != nil {
		return nil, err
	}
	out := map[string][]store.PublisherKey{}
	for _, k := range ks {
		out[k.PublisherID] = append(out[k.PublisherID], k)
	}
	return out, nil
}

// RemovePublisherKey removes a key: it stops working at once.
func (s *AuthAdmin) RemovePublisherKey(ctx context.Context, a authz.Actor, tenant, publisher, id string) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		p, err := tx.GetPublisher(ctx, t.ID, publisher)
		if err != nil {
			return err
		}
		if err := s.marked(ctx, a, p); err != nil {
			return err
		}
		fp, err := tx.DeletePublisherKey(ctx, p.ID, id)
		if err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "publisher.key.remove", "publisher:"+p.Name+"/key:"+id,
			map[string]any{"publisher": p.Name, "key": id, "fingerprint": fp})
	})
}

func (s *AuthAdmin) publisher(ctx context.Context, a authz.Actor, tenant, name string) (store.Publisher, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Publisher{}, err
	}
	var p store.Publisher
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		p, err = tx.GetPublisher(ctx, t.ID, name)
		return err
	})
	return p, err
}
