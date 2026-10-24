package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033: a tenant's catalogue entries arriving without a channel, their room, and their purge.

// typedEvent adds an event's package_type when it is not the default (spec 0034: events that name a
// package without a channel carry it then; an extension's events read as before).
func typedEvent(f map[string]any, packageType string) map[string]any {
	if packageType != "" && packageType != pkgtype.Default {
		f["package_type"] = packageType
	}
	return f
}

// KeepLanded is how many versions of a name from a source the catalogue keeps landed (placed
// nowhere); older ones are collected after the grace (spec 0016).
const KeepLanded = 3

// slotErr is a catalogue slot's refusal as its intake answers it: a mirror's cell conflicts, anything
// else is refused as a slot (409).
func slotErr(origin string, err error) error {
	if !errors.Is(err, store.ErrCatalogueSlot) {
		return err
	}
	if origin == store.OriginUpstream {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return fmt.Errorf("%w: %v", ErrSlot, err)
}

// room refuses a new build of a source whose catalogue backlog (builds placed nowhere) holds as
// many entries or bytes as it may (catalogue.max_entries, catalogue.max_bytes); a body the source
// has already is no new entry, and an administrator's add is not bounded. The count is read outside
// any lock: concurrent intakes may pass it together (bounded by the upload slots).
func (s *Service) room(ctx context.Context, b store.Build, size int64, admin bool) error {
	if admin || s.MaxEntries <= 0 && s.MaxBytes <= 0 {
		return nil
	}
	if _, ok, err := s.Store.BuildOf(ctx, b.TenantID, b.PackageType, b.Source, b.Name, b.BodyHash); err != nil || ok {
		return err
	}
	n, bytes, err := s.Store.SourceUsage(ctx, b.TenantID, b.Source)
	if err != nil {
		return err
	}
	if s.MaxEntries > 0 && n+1 > s.MaxEntries || s.MaxBytes > 0 && bytes+size > s.MaxBytes {
		return fmt.Errorf("%w (catalogue.max_entries, catalogue.max_bytes): the tenant's administrators place or purge its builds placed nowhere",
			store.ErrCatalogueFull)
	}
	return nil
}

// AddToCatalogue puts a built extension into a tenant's catalogue only, placed in no channel (spec
// 0033): a publication (the caller needs publish on the name tenant-wide; a reserved name through a
// grant naming it) or, without o.Publish, an administrator's add. The build's DuckDB version must be
// registered on the server. It returns the build and whether the source had it already.
func (s *Service) AddToCatalogue(ctx context.Context, a authz.Actor, tenant string, r io.Reader, o AddOptions) (store.Build, bool, error) {
	typ := o.PackageType
	if typ == "" {
		typ = pkgtype.Default
	}
	if !pkgtype.Known(typ) {
		return store.Build{}, false, fmt.Errorf("%w: package type %q", store.ErrInvalid, typ)
	}
	res := authz.Resource{Tenant: tenant}
	verb := authz.VerbAdmin
	dist := "" // spec 0033 phase B: a distributor's publisher's, which publishes without grants
	if o.Publish != nil {
		kind, err := s.Store.ReservedFor(ctx, tenant, typ, o.Name)
		if err != nil {
			return store.Build{}, false, err
		}
		o.Unchecked = false
		verb = authz.VerbPublish
		res = authz.Resource{Tenant: tenant, Package: o.Name, PackageType: typ, Reserved: kind != ""}
		if dist, err = s.distributorOf(ctx, a); err != nil {
			return store.Build{}, false, err
		}
	}
	if dist != "" {
		if err := s.distributorMay(ctx, tenant, typ, o.Name, dist); err != nil {
			return store.Build{}, false, err
		}
	} else if err := s.Authz.Allow(ctx, a, verb, res); err != nil {
		return store.Build{}, false, err
	}
	if typ != pkgtype.DuckDBExtension { // the intake reads DuckDB extensions (spec 0008); another type's is its spec's
		return store.Build{}, false, fmt.Errorf("%w: this server takes DuckDB extensions only, not %s", ErrPackageType, typ)
	}
	o.Unchecked = false // a build nothing could place would only hold its slot
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Build{}, false, err
	}
	spool := s.Blob.Spool
	if o.NoWait {
		spool = s.Blob.SpoolNoWait
	}
	sp, err := spool(ctx, t.StorageDomain, r)
	if err != nil {
		return store.Build{}, false, err
	}
	defer sp.Close()
	f := sp.File()
	b, err := BuildFrom(f, t.ID, o.Name, a.String())
	if err != nil {
		return store.Build{}, false, err
	}
	it := item{b: b, visibility: o.visibility(), origin: store.OriginAdmin, source: sourceOf(a)}
	it.b.Source = it.source
	if p := o.Publish; p != nil {
		if p.Version != b.PackageVersion || p.Platform != b.Platform {
			return store.Build{}, false, fmt.Errorf("%w: the file's footer says version %q, platform %q; the request says %q, %q",
				store.ErrInvalid, b.PackageVersion, b.Platform, p.Version, p.Platform)
		}
		it.origin, it.provenance = store.OriginPublication, p.Provenance
		it.b.Origin, it.b.PublisherKeyID = store.OriginPublication, p.KeyID
		trust := s.publisherKeys
		if dist != "" { // a distributor's publisher marked for licence keys may publish the name without a grant
			trust = s.distributorKeys
		}
		if err := trust(ctx, a, res, p.KeySources, f, &it); err != nil {
			return store.Build{}, false, err
		}
	}
	if !o.Unchecked {
		if err := CheckFile(f, o.Name); err != nil {
			return store.Build{}, false, err
		}
		it.b.Checked = true
	}
	if b.ABI != store.ABICStruct {
		if known, err := s.Store.DuckDBVersionKnown(ctx, b.DuckDBVersion); err != nil {
			return store.Build{}, false, err
		} else if !known {
			return store.Build{}, false, fmt.Errorf("%w: DuckDB %s is not registered on the server", store.ErrInvalid, b.DuckDBVersion)
		}
	}
	if blocked, err := s.Store.Blocked(ctx, t.ID, b.BodyHash); err != nil || blocked {
		if err == nil {
			err = ErrBlocked
		}
		return store.Build{}, false, err
	}
	_, existed, err := s.Store.BuildOf(ctx, t.ID, b.PackageType, it.source, b.Name, b.BodyHash)
	if err != nil {
		return store.Build{}, false, err
	}
	if !existed {
		if err := s.Store.SlotTaken(ctx, it.b); err != nil { // before the body is stored
			return store.Build{}, false, slotErr(it.origin, err)
		}
		if err := s.room(ctx, it.b, f.BodySize, o.Publish == nil); err != nil {
			return store.Build{}, false, err
		}
	}
	if dist != "" { // a name reserved during the upload, server-wide or for another holder (spec 0033 phase B)
		if err := s.distributorMay(ctx, tenant, typ, o.Name, dist); err != nil {
			return store.Build{}, false, err
		}
	} else if o.Publish != nil { // a name reserved during the upload takes a grant naming it (spec 0030)
		kind, err := s.Store.ReservedFor(ctx, tenant, typ, o.Name)
		if err != nil {
			return store.Build{}, false, err
		}
		if kind != "" && !res.Reserved {
			if err := s.reservedCheck(a, verb, res)(ctx); err != nil {
				return store.Build{}, false, err
			}
		}
	}
	label, err := s.Store.SourceLabel(ctx, it.source)
	if err != nil {
		return store.Build{}, false, err
	}
	created := false
	if err := s.land(ctx, sp, &it, func(tx *store.Tx, bb store.Build, inserted bool) error {
		if created = inserted; !inserted {
			return nil
		}
		f := map[string]any{"build": bb.ID, "name": bb.Name, "version": bb.PackageVersion, "platform": bb.Platform,
			"slot": bb.Slot(), "body_hash": bb.BodyHash, "source": label, "visibility": bb.Visibility}
		if it.provenance != "" {
			f["provenance"] = it.provenance
		}
		if !bb.AvailableAt.IsZero() { // spec 0033 phase A2: held until then
			f["available_at"] = bb.AvailableAt
		}
		typedEvent(f, bb.PackageType)
		return tx.Event(ctx, t.ID, a.String(), "catalogue.arrival", "ext:"+bb.Name, f)
	}); err != nil {
		return store.Build{}, false, slotErr(it.origin, err)
	}
	return it.b, !created, nil
}

// PurgeEntry deletes a tenant's build that no release holds (spec 0033): its slot is free again, and
// the collector removes its body. A tenant administrator's; a distribution admin's for a
// distributor's publisher's entry (spec 0033 phase B: removed or not). A withdrawn entry is never
// purged.
func (s *Service) PurgeEntry(ctx context.Context, a authz.Actor, tenant, id string) (store.Build, error) {
	t, err := s.mayReadCatalogue(ctx, a, tenant)
	if err != nil {
		return store.Build{}, err
	}
	only, err := s.distributorSources(ctx, a, t)
	if err != nil {
		return store.Build{}, err
	}
	var b store.Build
	// the tenant's upstream lock, which every release into its signed channels takes: no placement
	// releases the entry between the check and the deletion
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.TenantUpstreamLock(t.ID)}, func(tx *store.Tx) error {
		if only != nil {
			cur, err := tx.GetBuild(ctx, id)
			if err != nil {
				return err
			}
			if _, ok := only[cur.Source]; !ok || cur.TenantID != t.ID {
				return fmt.Errorf("%w: build %s", store.ErrNotFound, id) // a distribution admin's are its distributors' entries
			}
		}
		var err error
		if b, err = tx.DeleteEntry(ctx, t.ID, id); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "catalogue.purge", "ext:"+b.Name, typedEvent(map[string]any{"build": b.ID, "name": b.Name,
			"version": b.PackageVersion, "platform": b.Platform, "slot": b.Slot(), "body_hash": b.BodyHash}, b.PackageType))
	})
	return b, err
}

// TrimLanded lets the collector take landed builds the catalogue keeps no more: of a publisher or the
// tenant, those older than the KeepLanded newest landed versions of their (source, name, platform,
// slot); of a mirror, those its cell no longer holds (the cell moved on to another body, or the
// upstream is gone). It returns how many.
func (s *Service) TrimLanded(ctx context.Context) (int, error) {
	crowded, err := s.Store.CrowdedLanded(ctx, KeepLanded)
	if err != nil {
		return 0, err
	}
	by := map[[6]string][]store.LandedBuild{}
	for _, b := range crowded {
		k := [6]string{b.TenantID, b.PackageType, b.Source, b.Name, b.Platform, b.Slot}
		by[k] = append(by[k], b)
	}
	var drop []string
	for _, group := range by {
		var versions []string
		for _, b := range group {
			if !slices.Contains(versions, b.Version) {
				versions = append(versions, b.Version)
			}
		}
		slices.SortFunc(versions, func(x, y string) int { return CompareVersions(y, x) })
		keep := versions[:min(KeepLanded, len(versions))]
		for _, b := range group {
			if !slices.Contains(keep, b.Version) {
				drop = append(drop, b.ID)
			}
		}
	}
	stale, err := s.Store.StaleMirrorLanded(ctx)
	if err != nil {
		return 0, err
	}
	for _, b := range stale {
		drop = append(drop, b.ID)
	}
	return len(drop), s.Store.Unland(ctx, drop)
}
