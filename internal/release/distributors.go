package release

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033 phase B part 1: distributors' publishers in a tenant's catalogue, and their withdrawals.

// distributorOf is the distributor of a publisher actor's publisher (spec 0033 phase B), "" for any
// other caller or a publisher of the tenant's own.
func (s *Service) distributorOf(ctx context.Context, a authz.Actor) (string, error) {
	if a.Kind != authz.ActorPublisher {
		return "", nil
	}
	src := sourceOf(a)
	if !strings.HasPrefix(src, store.SourcePublisherPrefix) {
		return "", nil
	}
	return s.Store.SourceDistributor(ctx, src)
}

// distributorMay says whether a distributor's publisher may publish a name into a tenant's catalogue
// (spec 0033 phase B): any name kista does not reserve server-wide and that the tenant does not
// reserve for another holder than its distributor (spec 0030). It needs no grant: what it publishes
// reaches no channel by itself. A refusal is authz.ErrDenied (nothing said, as for a missing grant).
func (s *Service) distributorMay(ctx context.Context, tenant, packageType, name, dist string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return err
	}
	kind, err := s.Store.NameReservedFor(ctx, t.ID, packageType, name)
	if err != nil {
		return err
	}
	switch kind {
	case "":
		return nil
	case "tenant":
		holder, ok, err := s.Store.TenantReservedHolder(ctx, t.ID, packageType, name)
		if err != nil {
			return err
		}
		if !ok || holder == dist {
			return nil
		}
	}
	return authz.ErrDenied
}

// distributorMayNow is distributorMay inside the build's landing transaction (spec 0033 phase B): the
// tenant still active (not suspended or deleting meanwhile), and the name not reserved server-wide
// nor in the tenant for another holder since the upload began.
func distributorMayNow(ctx context.Context, tx *store.Tx, tenantID, packageType, name, dist string) error {
	if active, err := tx.TenantActive(ctx, tenantID); err != nil {
		return err
	} else if !active {
		return authz.ErrDenied
	}
	if k, err := tx.ReservedKind(ctx, packageType, name); err != nil {
		return err
	} else if k != "" {
		return authz.ErrDenied
	}
	holder, held, err := tx.TenantReservedHolder(ctx, tenantID, packageType, name)
	if err != nil {
		return err
	}
	if held && holder != dist {
		return authz.ErrDenied
	}
	return nil
}

// mayDistribute admits a distribution admin (distribute on the tenant) or a server administrator
// (spec 0033 phase B: a distributor's withdrawals are theirs, never the tenant's).
func (s *Service) mayDistribute(ctx context.Context, a authz.Actor, tenant string) (store.Tenant, error) {
	verb, res := authz.VerbAdmin, authz.Server
	if a.Kind == authz.ActorDistributionAdmin {
		verb, res = authz.VerbDistribute, authz.Resource{Tenant: tenant}
	}
	if err := s.Authz.Allow(ctx, a, verb, res); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, tenant)
}

// Withdrawal is what a withdrawal did (spec 0033 phase B): the entries it withdrew, and whether the
// version was withdrawn already. It names no channel: the distributor learns nothing about them.
type Withdrawal struct {
	Tenant, Source, Name, Version, Reason string
	PackageType                           string
	Builds                                []store.Build
	Already                               bool
}

// Withdraw withdraws a version of a name from a distributor's publisher's source (removed or not; named
// publisher:<name>, or publisher-id:<id> when the name is ambiguous) in a tenant, with a reason (spec
// 0033 phase B): a distribution admin's or a server administrator's. In one transaction the version
// is withdrawn for good from the source, whether it holds entries of it or not (none arrives again),
// its entries become withdrawn and every channel of the tenant holding a release of them gets its
// release version bumped; such a release is served nowhere from then on and never made active again.
// The tenant's administrators are told (catalogue.withdrawn). Withdrawing a version withdrawn already
// changes nothing.
func (s *Service) Withdraw(ctx context.Context, a authz.Actor, tenant, packageType, source, name, version, reason string) (Withdrawal, error) {
	t, err := s.mayDistribute(ctx, a, tenant)
	if err != nil {
		return Withdrawal{}, err
	}
	if packageType == "" {
		packageType = pkgtype.Default
	}
	if !pkgtype.Known(packageType) {
		return Withdrawal{}, fmt.Errorf("%w: package type %q", store.ErrInvalid, packageType)
	}
	if version == "" {
		return Withdrawal{}, fmt.Errorf("%w: a version is required", store.ErrInvalid)
	}
	if err := ValidName(name); err != nil {
		return Withdrawal{}, err
	}
	src, err := s.Store.ResolveDistributorSource(ctx, t.ID, source)
	if errors.Is(err, store.ErrNoSource) {
		return Withdrawal{}, fmt.Errorf("%w: no distributor's publisher %q in the tenant", store.ErrNotFound, source)
	}
	if err != nil {
		return Withdrawal{}, err
	}
	out := Withdrawal{Tenant: t.Name, Source: source, Name: name, Version: version, Reason: reason, PackageType: packageType}
	// the tenant's upstream lock: every release into its signed channels takes it, so no placement
	// commits between the withdrawal's update and its read of the channels (the entry's row lock, which
	// a placement reads with, makes it so too); then its auth lock, which a publication's build
	// transaction takes, so no build of the version lands unseen (the order of the purging removals)
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.TenantUpstreamLock(t.ID), store.PublisherAuthLock(t.ID)}, func(tx *store.Tx) error {
		w, already, err := tx.Withdraw(ctx, t.ID, packageType, src, name, version, reason, a.String())
		if err != nil {
			return err
		}
		out.Builds, out.Already = w.Builds, already
		if already {
			return nil
		}
		ids := make([]string, len(w.Builds))
		for i, b := range w.Builds {
			ids[i] = b.ID
		}
		return tx.Event(ctx, t.ID, a.String(), "catalogue.withdrawn", "ext:"+name, typedEvent(map[string]any{"source": source,
			"name": name, "version": version, "reason": reason, "builds": ids, "channels": len(w.Channels)}, packageType))
	})
	return out, err
}

// distributorSources are the sources a caller reads and purges in a tenant's catalogue as a
// distribution admin (spec 0033 phase B): the distributors' publishers, live or removed, by source;
// nil for any other caller (its own rules apply).
func (s *Service) distributorSources(ctx context.Context, a authz.Actor, t store.Tenant) (map[string]store.DistributorSource, error) {
	if a.Kind != authz.ActorDistributionAdmin {
		return nil, nil
	}
	return s.Store.DistributorSources(ctx, t.ID)
}

// mayReadCatalogue admits a tenant's administrators, and a distribution admin to its distributors'
// entries only (spec 0033 phase B).
func (s *Service) mayReadCatalogue(ctx context.Context, a authz.Actor, tenant string) (store.Tenant, error) {
	if a.Kind == authz.ActorDistributionAdmin {
		if err := s.Authz.Allow(ctx, a, authz.VerbDistribute, authz.Resource{Tenant: tenant}); err != nil {
			return store.Tenant{}, err
		}
		return s.Store.GetTenant(ctx, tenant)
	}
	return s.mayAdminTenant(ctx, a, tenant)
}
