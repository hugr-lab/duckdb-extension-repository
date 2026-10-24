package tenants

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Publishers (spec 0008): identities that only publish and promote, authenticated by their keys (spec 0023).

// AddPublisher adds a publisher to a tenant.
func (s *AuthAdmin) AddPublisher(ctx context.Context, a authz.Actor, tenant, name string) (store.Publisher, error) {
	t, err := s.tenantOrDistributor(ctx, a, tenant)
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

// ListPublishers lists a tenant's publishers.
func (s *AuthAdmin) ListPublishers(ctx context.Context, a authz.Actor, tenant string) ([]store.Publisher, error) {
	t, err := s.tenantOrDistributor(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	ta, err := s.Store.GetTenantAuth(ctx, t.ID)
	return ta.Publishers, err
}

// RemovePublisher removes a publisher, its keys and its grants, and purges the entries it published
// that are still held (spec 0033 phase A2); those placed early are kept, revoked. It returns what
// the purge did.
func (s *AuthAdmin) RemovePublisher(ctx context.Context, a authz.Actor, tenant, name string) (HeldPurge, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return HeldPurge{}, err
	}
	var hp HeldPurge
	// the tenant's upstream lock orders the purge against every release into its signed channels
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.PublisherAuthLock(t.ID), store.TenantUpstreamLock(t.ID)}, func(tx *store.Tx) error {
		p, err := tx.GetPublisher(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if hp, err = purgeHeld(ctx, tx, a, t.ID, p, "", "publisher.remove"); err != nil {
			return err
		}
		if err := tx.DeletePublisher(ctx, t.ID, name); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "publisher.remove", "publisher:"+name, hp.event(map[string]any{"name": name}))
	})
	return hp, err
}

// HeldPurge is what revoking a key with purge_held, or removing a publisher, did to the entries it
// published that were held (spec 0033 phase A2): those purged, and those kept (placed early, or
// released once), now revoked.
type HeldPurge struct {
	Purged []store.Build
	Kept   []store.KeptEntry
}

// event adds the purge to its removal's event: how many entries it purged, and the entries it kept
// with how many of the tenant's channels hold them.
func (hp HeldPurge) event(f map[string]any) map[string]any {
	f["purged"] = len(hp.Purged)
	if len(hp.Kept) > 0 {
		kept := make([]map[string]any, len(hp.Kept))
		for i, k := range hp.Kept {
			kept[i] = map[string]any{"build": k.ID, "name": k.Name, "version": k.PackageVersion, "platform": k.Platform, "slot": k.Slot(),
				"channels": k.Channels}
			if k.PackageType != pkgtype.Default { // spec 0034: an extension's events read as before
				kept[i]["package_type"] = k.PackageType
			}
		}
		f["kept"] = kept
	}
	return f
}

// purgeHeld purges a publisher's entries that are still held (of one of its keys when keyID is set),
// each with a catalogue.purge event naming why (spec 0033 phase A2: a hold is what lets a stolen key
// be revoked before its builds reach a channel), and marks those it keeps revoked. The caller holds
// the tenant's upstream lock (store.PurgeHeld).
func purgeHeld(ctx context.Context, tx *store.Tx, a authz.Actor, tenantID string, p store.Publisher, keyID, reason string) (HeldPurge, error) {
	bs, kept, err := tx.PurgeHeld(ctx, tenantID, store.PublisherSource(p.ID), keyID)
	if err != nil {
		return HeldPurge{}, err
	}
	for _, b := range bs {
		f := map[string]any{"build": b.ID, "name": b.Name, "version": b.PackageVersion, "platform": b.Platform, "slot": b.Slot(),
			"body_hash": b.BodyHash, "reason": reason}
		if keyID != "" {
			f["key"] = keyID
		}
		if b.PackageType != pkgtype.Default { // spec 0034: an extension's events read as before
			f["package_type"] = b.PackageType
		}
		if err := tx.Event(ctx, tenantID, a.String(), "catalogue.purge", "ext:"+b.Name, f); err != nil {
			return HeldPurge{}, err
		}
	}
	return HeldPurge{Purged: bs, Kept: kept}, nil
}

// SetPublisherHold sets a publisher's hold (spec 0033 phase A2), a tenant administrator's: an entry
// it publishes is held until its arrival plus the hold, and it publishes into the catalogue only (the
// channel route refuses it). Raising it applies to the entries held now; lowering it, to those
// arriving from then on. (A distributor's publisher's hold is a distribution admin's: spec 0033
// phase B, with the distributor mark.)
func (s *AuthAdmin) SetPublisherHold(ctx context.Context, a authz.Actor, tenant, name string, d time.Duration) (store.Publisher, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Publisher{}, err
	}
	var p store.Publisher
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		before, err := tx.SetPublisherHold(ctx, t.ID, name, d)
		if err != nil {
			return err
		}
		p = before
		p.Hold = d
		held := 0
		if d > before.Hold {
			if held, err = tx.RaiseHeld(ctx, t.ID, store.PublisherSource(p.ID), d); err != nil {
				return err
			}
		}
		return tx.Event(ctx, t.ID, a.String(), "catalogue.hold", "publisher:"+p.Name,
			map[string]any{"source": "publisher:" + p.Name, "hold": d.String(), "before": before.Hold.String(), "held": held})
	})
	return p, err
}

// SetPublisherLicenseKeys marks a publisher a trusted source of licence keys, or unmarks it (spec
// 0021): a server administrator's act, or a distribution admin's (spec 0030). While marked, its keys
// and grants are theirs to change; unmarking keeps the keys it brought.
func (s *AuthAdmin) SetPublisherLicenseKeys(ctx context.Context, a authz.Actor, tenant, name string, on bool) (store.Publisher, error) {
	verb, res := authz.VerbAdmin, authz.Server
	if distributor(a) {
		verb, res = authz.VerbDistribute, authz.Resource{Tenant: tenant}
	}
	if err := s.Authz.Allow(ctx, a, verb, res); err != nil {
		return store.Publisher{}, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Publisher{}, err
	}
	var p store.Publisher
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		if on {
			// keys a tenant administrator registered before the mark would outlive it: the server
			// administrator adds them once it is marked
			cur, err := tx.GetPublisher(ctx, t.ID, name)
			if err != nil {
				return err
			}
			if distributor(a) && !cur.LicenseKeys {
				// spec 0030: a distribution admin marks a publisher of its own making, before it holds
				// keys or grants; a tenant's publisher with grants would become the distributor's
				if n, err := tx.PublisherGrants(ctx, cur.ID); err != nil {
					return err
				} else if n > 0 {
					return fmt.Errorf("%w: publisher %s holds grants (%d): a distribution admin marks a publisher before it has any",
						store.ErrInvalid, name, n)
				}
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

// marked refuses a change to a marked publisher's keys or grants unless a server
// administrator or a distribution admin makes it (spec 0021, 0030): a tenant administrator cannot
// point it at builds of their own. A distribution admin changes marked publishers only: the
// tenant's own publishers are its administrators'.
func (s *AuthAdmin) marked(ctx context.Context, a authz.Actor, p store.Publisher) error {
	if distributor(a) {
		if !p.LicenseKeys {
			return authz.ErrDenied
		}
		return nil // the call admitted it on the tenant
	}
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
	t, err := s.tenantOrDistributor(ctx, a, tenant)
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
	if distributor(a) && !p.LicenseKeys {
		return nil, authz.ErrDenied // a distribution admin's are the marked publishers
	}
	return s.Store.PublisherKeys(ctx, p.TenantID, p.ID)
}

// TenantPublisherKeys lists every key of a tenant's publishers, by publisher id.
func (s *AuthAdmin) TenantPublisherKeys(ctx context.Context, a authz.Actor, tenant string) (map[string][]store.PublisherKey, error) {
	t, err := s.tenantOrDistributor(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	ks, err := s.Store.PublisherKeys(ctx, t.ID, "")
	if err != nil {
		return nil, err
	}
	marked := map[string]bool{}
	if distributor(a) { // a distribution admin's are the marked publishers
		ta, err := s.Store.GetTenantAuth(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range ta.Publishers {
			marked[p.ID] = p.LicenseKeys
		}
	}
	out := map[string][]store.PublisherKey{}
	for _, k := range ks {
		if distributor(a) && !marked[k.PublisherID] {
			continue
		}
		out[k.PublisherID] = append(out[k.PublisherID], k)
	}
	return out, nil
}

// RemovePublisherKey removes a key: it stops working at once. With purge, the entries published
// with it that are still held are purged, and those placed early kept, revoked (spec 0033 phase A2:
// purge_held; a revoked key's builds reach no channel by themselves); a routine rotation, without
// it, keeps them. purge is a tenant administrator's: a distribution admin asking for it is refused
// (spec 0033 phase B designs a distributor's purges). It returns what the purge did.
func (s *AuthAdmin) RemovePublisherKey(ctx context.Context, a authz.Actor, tenant, publisher, id string, purge bool) (HeldPurge, error) {
	if purge && distributor(a) {
		return HeldPurge{}, authz.ErrDenied
	}
	t, err := s.tenantOrDistributor(ctx, a, tenant)
	if err != nil {
		return HeldPurge{}, err
	}
	locks := []string{store.PublisherAuthLock(t.ID)}
	if purge { // the tenant's upstream lock orders the purge against every release into its signed channels
		locks = append(locks, store.TenantUpstreamLock(t.ID))
	}
	var hp HeldPurge
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, locks, func(tx *store.Tx) error {
		p, err := tx.GetPublisher(ctx, t.ID, publisher)
		if err != nil {
			return err
		}
		if err := s.marked(ctx, a, p); err != nil {
			return err
		}
		k, err := tx.DeletePublisherKey(ctx, p.ID, id)
		if err != nil {
			return err
		}
		f := map[string]any{"publisher": p.Name, "key": id, "fingerprint": k.Fingerprint, "type": k.Type, "title": k.Title, "purge_held": purge}
		if purge {
			if hp, err = purgeHeld(ctx, tx, a, t.ID, p, id, "publisher.key.remove"); err != nil {
				return err
			}
			f = hp.event(f)
		}
		return tx.Event(ctx, t.ID, a.String(), "publisher.key.remove", "publisher:"+p.Name+"/key:"+id, f)
	})
	return hp, err
}

func (s *AuthAdmin) publisher(ctx context.Context, a authz.Actor, tenant, name string) (store.Publisher, error) {
	t, err := s.tenantOrDistributor(ctx, a, tenant)
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
