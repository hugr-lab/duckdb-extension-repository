package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033: a tenant's catalogue entries arriving without a channel, their room, and their purge.

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

// room refuses a new build of a source that holds as many entries or bytes as it may
// (catalogue.max_entries, catalogue.max_bytes); a body the source has already is no new entry.
func (s *Service) room(ctx context.Context, b store.Build, size int64) error {
	if s.MaxEntries <= 0 && s.MaxBytes <= 0 {
		return nil
	}
	if _, ok, err := s.Store.BuildOf(ctx, b.TenantID, b.Source, b.Name, b.BodyHash); err != nil || ok {
		return err
	}
	n, bytes, err := s.Store.SourceUsage(ctx, b.TenantID, b.Source)
	if err != nil {
		return err
	}
	if s.MaxEntries > 0 && n+1 > s.MaxEntries || s.MaxBytes > 0 && bytes+size > s.MaxBytes {
		return fmt.Errorf("%w: %d builds, %d bytes (catalogue.max_entries %d, catalogue.max_bytes %d); purge older entries",
			store.ErrCatalogueFull, n, bytes, s.MaxEntries, s.MaxBytes)
	}
	return nil
}

// AddToCatalogue puts a built extension into a tenant's catalogue only, placed in no channel (spec
// 0033): a publication (the caller needs publish on the name tenant-wide; a reserved name through a
// grant naming it) or, without o.Publish, an administrator's add. The build's DuckDB version must be
// registered on the server. It returns the build and whether the source had it already.
func (s *Service) AddToCatalogue(ctx context.Context, a authz.Actor, tenant string, r io.Reader, o AddOptions) (store.Build, bool, error) {
	res := authz.Resource{Tenant: tenant}
	verb := authz.VerbAdmin
	if o.Publish != nil {
		kind, err := s.Store.ReservedFor(ctx, tenant, o.Name)
		if err != nil {
			return store.Build{}, false, err
		}
		o.Unchecked = false
		verb = authz.VerbPublish
		res = authz.Resource{Tenant: tenant, Extension: o.Name, Reserved: kind != ""}
	}
	if err := s.Authz.Allow(ctx, a, verb, res); err != nil {
		return store.Build{}, false, err
	}
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
		if p.Version != b.ExtVersion || p.Platform != b.Platform {
			return store.Build{}, false, fmt.Errorf("%w: the file's footer says version %q, platform %q; the request says %q, %q",
				store.ErrInvalid, b.ExtVersion, b.Platform, p.Version, p.Platform)
		}
		it.origin, it.provenance = store.OriginPublication, p.Provenance
		it.b.Origin = store.OriginPublication
		if err := s.publisherKeys(ctx, a, res, p.KeySources, f, &it); err != nil {
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
	_, existed, err := s.Store.BuildOf(ctx, t.ID, it.source, b.Name, b.BodyHash)
	if err != nil {
		return store.Build{}, false, err
	}
	if err := s.room(ctx, it.b, f.BodySize); err != nil {
		return store.Build{}, false, err
	}
	if err := s.land(ctx, sp, &it); err != nil {
		return store.Build{}, false, slotErr(it.origin, err)
	}
	if !existed {
		label, err := s.Store.SourceLabel(ctx, it.source)
		if err != nil {
			return store.Build{}, false, err
		}
		if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
			f := map[string]any{"build": it.b.ID, "name": it.b.Name, "version": it.b.ExtVersion, "platform": it.b.Platform,
				"slot": it.b.Slot(), "body_hash": it.b.BodyHash, "source": label, "visibility": it.b.Visibility}
			if it.provenance != "" {
				f["provenance"] = it.provenance
			}
			return tx.Event(ctx, t.ID, a.String(), "catalogue.arrival", "ext:"+it.b.Name, f)
		}); err != nil {
			return store.Build{}, false, err
		}
	}
	return it.b, existed, nil
}

// PurgeEntry deletes a tenant's build that no release holds (spec 0033): its slot is free again, and
// the collector removes its body. A tenant administrator's.
func (s *Service) PurgeEntry(ctx context.Context, a authz.Actor, tenant, id string) (store.Build, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Build{}, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Build{}, err
	}
	var b store.Build
	err = s.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		var err error
		if b, err = tx.DeleteEntry(ctx, t.ID, id); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "catalogue.purge", "ext:"+b.Name, map[string]any{"build": b.ID, "name": b.Name,
			"version": b.ExtVersion, "platform": b.Platform, "slot": b.Slot(), "body_hash": b.BodyHash})
	})
	return b, err
}

// TrimLanded lets the collector take landed builds older than the KeepLanded newest versions of
// their (tenant, source, name). It returns how many.
func (s *Service) TrimLanded(ctx context.Context) (int, error) {
	bs, err := s.Store.LandedBuilds(ctx)
	if err != nil {
		return 0, err
	}
	by := map[[3]string][]store.LandedBuild{}
	for _, b := range bs {
		k := [3]string{b.TenantID, b.Source, b.Name}
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
		if len(versions) <= KeepLanded {
			continue
		}
		slices.SortFunc(versions, func(x, y string) int { return CompareVersions(y, x) })
		keep := versions[:KeepLanded]
		for _, b := range group {
			if !slices.Contains(keep, b.Version) {
				drop = append(drop, b.ID)
			}
		}
	}
	return len(drop), s.Store.Unland(ctx, drop)
}
