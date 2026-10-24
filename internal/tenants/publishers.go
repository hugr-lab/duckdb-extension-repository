package tenants

import (
	"context"
	"fmt"
	"strings"
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

// AddDistributorPublisher adds a publisher to a tenant marked a distributor's from its creation (spec
// 0033 phase B): a server administrator's or a distribution admin's. This is how a server
// administrator makes a distributor's publisher: an existing one is marked only when a distribution
// admin created it (SetPublisherDistributor).
func (s *AuthAdmin) AddDistributorPublisher(ctx context.Context, a authz.Actor, tenant, name, dist string) (store.Publisher, error) {
	t, err := s.distributorAct(ctx, a, tenant)
	if err != nil {
		return store.Publisher{}, err
	}
	p := store.Publisher{TenantID: t.ID, Name: name, CreatedBy: a.String()}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.PublisherAuthLock(t.ID)}, func(tx *store.Tx) error {
		if err := tx.InsertPublisher(ctx, &p); err != nil {
			return err
		}
		if _, err := tx.SetPublisherDistributor(ctx, t.ID, name, dist); err != nil {
			return err
		}
		p.Distributor = dist
		return tx.Event(ctx, t.ID, a.String(), "publisher.add", "publisher:"+p.Name, map[string]any{"name": p.Name, "distributor": dist})
	})
	return p, err
}

// distributorAct admits a server administrator, or a distribution admin on the tenant (spec 0033
// phase B: a distributor's publisher's mark), and reads the tenant.
func (s *AuthAdmin) distributorAct(ctx context.Context, a authz.Actor, tenant string) (store.Tenant, error) {
	verb, res := authz.VerbAdmin, authz.Server
	if distributor(a) {
		verb, res = authz.VerbDistribute, authz.Resource{Tenant: tenant}
	}
	if err := s.Authz.Allow(ctx, a, verb, res); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, tenant)
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
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, purgeLocks(t.ID), func(tx *store.Tx) error {
		p, err := tx.GetPublisher(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if hp, err = purgeHeld(ctx, tx, a, t.ID, p, "", "publisher.remove"); err != nil {
			return err
		}
		if err := tx.DeletePublisher(ctx, t.ID, name, a.String()); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "publisher.remove", "publisher:"+name, hp.event(map[string]any{"name": name}))
	})
	return hp, err
}

// purgeLocks are the locks of a removal that purges held entries (spec 0033 phase A2): the tenant's
// upstream lock, which every release into its signed channels takes (store.PurgeHeld), then its auth
// lock. Upstream first: a release's insert holds the upstream lock and may wait, on another
// connection, for the auth lock (its grant re-checked, GetTenantAuth's locked read), so a removal
// holding the auth lock must never wait for the upstream lock.
func purgeLocks(tenantID string) []string {
	return []string{store.TenantUpstreamLock(tenantID), store.PublisherAuthLock(tenantID)}
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
// arriving from then on. A distributor's publisher's hold is a distribution admin's or a server
// administrator's only (spec 0033 phase B): a tenant that wants more waits with a channel hold.
func (s *AuthAdmin) SetPublisherHold(ctx context.Context, a authz.Actor, tenant, name string, d time.Duration) (store.Publisher, error) {
	t, err := s.tenantOrDistributor(ctx, a, tenant)
	if err != nil {
		return store.Publisher{}, err
	}
	var p store.Publisher
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{"kista/tenant-auth/" + t.ID}, func(tx *store.Tx) error {
		cur, err := tx.GetPublisher(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if err := s.distributors(ctx, a, cur); err != nil {
			return err
		}
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
			} else if n > 0 && !cur.LicenseKeys && cur.Distributor == "" { // a distributor's keys are its distributor's already
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

// SetPublisherDistributor marks an existing publisher a distributor's (spec 0033 phase B), a
// distribution admin's or a server administrator's act: distributor is the distributor's name, as a
// tenant reservation names its holder. A distributor's publisher publishes into the catalogue only,
// without grants (any name kista does not reserve server-wide and the tenant does not reserve for
// another holder), and reaches no channel by itself; its keys, its hold, its withdrawals and the
// purges of its entries are the distributor's. Only a fresh publisher a distribution admin created is
// marked so: no grants, no keys unless it carries the licence-key mark, no hold, never a source of
// anything (a publisher created otherwise, a tenant administrator's or an operator's, is refused as
// not found; a server administrator creates a distributor's publisher marked, AddDistributorPublisher).
// Marking a distributor's publisher again changes the distributor's name. A mark is never removed: the
// publisher is removed instead (its record kept, so its versions can still be withdrawn).
func (s *AuthAdmin) SetPublisherDistributor(ctx context.Context, a authz.Actor, tenant, name, dist string) (store.Publisher, error) {
	t, err := s.distributorAct(ctx, a, tenant)
	if err != nil {
		return store.Publisher{}, err
	}
	var p store.Publisher
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.PublisherAuthLock(t.ID)}, func(tx *store.Tx) error {
		cur, err := tx.GetPublisher(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if cur.Distributor == "" { // a fresh publisher only; marked already, it is renamed
			if !strings.HasPrefix(cur.CreatedBy, string(authz.ActorDistributionAdmin)+":") {
				return authz.ErrDenied // the tenant's own publisher, or an operator's, is never made the distributor's
			}
			grants, err := tx.PublisherGrants(ctx, cur.ID)
			if err != nil {
				return err
			}
			keys, err := tx.PublisherCredentials(ctx, cur.ID)
			if err != nil {
				return err
			}
			used, err := tx.PublisherUsed(ctx, cur)
			if err != nil {
				return err
			}
			if grants > 0 || keys > 0 && !cur.LicenseKeys || used {
				return fmt.Errorf("%w: publisher %s is not fresh: a distributor's publisher is marked before it holds grants, keys "+
					"(unless marked for licence keys), a hold, builds, releases or rules; add a new one", store.ErrInvalid, name)
			}
		}
		before, err := tx.SetPublisherDistributor(ctx, t.ID, name, dist)
		if err != nil {
			return err
		}
		p = before
		p.Distributor = dist
		if before.Distributor == dist {
			return nil
		}
		f := map[string]any{"name": p.Name, "distributor": dist}
		if before.Distributor != "" {
			f["before"] = before.Distributor
		}
		return tx.Event(ctx, t.ID, a.String(), "publisher.change", "publisher:"+p.Name, f)
	})
	return p, err
}

// distributors refuses a change to a distributor's publisher (its hold) unless a distribution admin
// or a server administrator makes it (spec 0033 phase B), and a distribution admin's change to any
// other publisher.
func (s *AuthAdmin) distributors(ctx context.Context, a authz.Actor, p store.Publisher) error {
	if distributor(a) {
		if p.Distributor == "" {
			return authz.ErrDenied
		}
		return nil // the call admitted it on the tenant
	}
	if p.Distributor == "" {
		return nil
	}
	return s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server)
}

// marked refuses a change to a marked publisher's keys or grants unless a server
// administrator or a distribution admin makes it (spec 0021, 0030): a tenant administrator cannot
// point it at builds of their own. A distribution admin changes marked publishers only: the
// tenant's own publishers are its administrators'. A distributor's publisher (spec 0033 phase B) is
// marked so too.
func (s *AuthAdmin) marked(ctx context.Context, a authz.Actor, p store.Publisher) error {
	isMarked := p.LicenseKeys || p.Distributor != ""
	if distributor(a) {
		if !isMarked {
			return authz.ErrDenied
		}
		return nil // the call admitted it on the tenant
	}
	if !isMarked {
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
	if distributor(a) && !p.LicenseKeys && p.Distributor == "" {
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
			marked[p.ID] = p.LicenseKeys || p.Distributor != ""
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
// it, keeps them. A distribution admin purges with a distributor's publisher's keys only (spec 0033
// phase B: a licence-key mark alone gives it the keys, not the purges). It returns what the purge did.
func (s *AuthAdmin) RemovePublisherKey(ctx context.Context, a authz.Actor, tenant, publisher, id string, purge bool) (HeldPurge, error) {
	t, err := s.tenantOrDistributor(ctx, a, tenant)
	if err != nil {
		return HeldPurge{}, err
	}
	locks := []string{store.PublisherAuthLock(t.ID)}
	if purge {
		locks = purgeLocks(t.ID)
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
		if purge && distributor(a) && p.Distributor == "" {
			return authz.ErrDenied
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
