package tenants

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0030: the server's reserved names, as an administrator sees and changes them. A version's
// core extensions come with the version (AddVersion), an upstream's names with its intake; these
// are the names an administrator adds, and removes again.

// ListReserved lists the reserved names with their sources (one name, or all when name is "").
func (s *Service) ListReserved(ctx context.Context, a authz.Actor, name string) ([]store.ReservedName, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return nil, err
	}
	return s.Store.ListReservedNames(ctx, name)
}

// Reserved is what an administrator's reservation did: the names that were not reserved as kind
// before, and the tenants that keep releases under them (by name).
type Reserved struct {
	Added   []string
	Tenants map[string][]string
}

// Reserve reserves names as core or community, with the source admin:<actor>. A name with releases
// that are not yanked in some tenant is refused unless forced: the tenants keep those releases
// (spec 0008), and only grants naming the extension reach the name from then on.
func (s *Service) Reserve(ctx context.Context, a authz.Actor, names []string, kind string, force bool) (Reserved, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return Reserved{}, err
	}
	if kind != store.ReservedCore && kind != store.ReservedCommunity {
		return Reserved{}, fmt.Errorf("%w: kind %q (core or community)", store.ErrInvalid, kind)
	}
	if len(names) == 0 {
		return Reserved{}, fmt.Errorf("%w: no names", store.ErrInvalid)
	}
	names = slices.Clone(names)
	slices.Sort(names)
	names = slices.Compact(names)
	for _, n := range names {
		if !coreNameRE.MatchString(n) {
			return Reserved{}, fmt.Errorf("%w: %q is not an extension name", store.ErrInvalid, n)
		}
	}
	held, err := s.held(ctx, names, kind)
	if err != nil {
		return Reserved{}, err
	}
	res := Reserved{Tenants: held}
	if err := heldRefusal(held, force); err != nil {
		return res, err
	}
	src := adminSource(a)
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		res.Added = nil
		for _, n := range names {
			changed, err := tx.ReserveName(ctx, n, kind, src)
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
			res.Added = append(res.Added, n)
			data := map[string]any{"kind": kind, "names": []string{n}, "source": src}
			if ts := res.Tenants[n]; len(ts) > 0 {
				data["forced"], data["tenants"] = true, ts
			}
			if err := tx.Event(ctx, "", a.String(), "reserved.add", "reserved:"+n, data); err != nil {
				return err
			}
		}
		if len(res.Added) > 0 {
			return tx.BumpReservedVersion(ctx)
		}
		return nil
	})
	return res, err
}

// held lists, by name, the tenants with releases that are not yanked under the names a reservation
// as kind would change (a name not reserved, or community becoming core): those releases stay, and
// only grants naming the extension reach the name from then on. A name whose kind stays is left out.
func (s *Service) held(ctx context.Context, names []string, kind string) (map[string][]string, error) {
	kinds, _, err := s.Store.ReservedNames(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, n := range names {
		if k := kinds[n]; k == kind || k == store.ReservedCore {
			continue
		}
		ts, err := s.Store.TenantsWithReleases(ctx, n)
		if err != nil {
			return nil, err
		}
		if len(ts) > 0 {
			out[n] = ts
		}
	}
	return out, nil
}

// heldRefusal refuses a reservation over tenants' releases unless forced.
func heldRefusal(held map[string][]string, force bool) error {
	if len(held) == 0 || force {
		return nil
	}
	var parts []string
	for _, n := range slices.Sorted(maps.Keys(held)) {
		parts = append(parts, n+" ("+strings.Join(held[n], ", ")+")")
	}
	return &Refusal{Reason: fmt.Sprintf("releases under %s: they stay, and only grants naming the extension reach the name; add with force",
		strings.Join(parts, "; "))}
}

// refuseHeld is heldRefusal for a reservation that answers no tenants (a version's core extensions).
func (s *Service) refuseHeld(ctx context.Context, names []string, kind string, force bool) error {
	held, err := s.held(ctx, names, kind)
	if err != nil {
		return err
	}
	return heldRefusal(held, force)
}

// Unreserve removes a name only administrators reserved; a name DuckDB brought (a version's, an
// upstream's, the seed's) stays.
func (s *Service) Unreserve(ctx context.Context, a authz.Actor, name string) error {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return err
	}
	return s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		sources, err := tx.RemoveReservedName(ctx, name)
		if errors.Is(err, store.ErrConflict) {
			return &Refusal{Reason: strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": ")}
		}
		if err != nil {
			return err
		}
		return tx.Event(ctx, "", a.String(), "reserved.remove", "reserved:"+name, map[string]any{"name": name, "sources": sources})
	})
}

// adminSource is an administrator's source, admin:<actor>, in the sources' printable ASCII.
func adminSource(a authz.Actor) string {
	b := []byte("admin:" + a.String())
	for i, c := range b {
		if c < 0x21 || c > 0x7e {
			b[i] = '_'
		}
	}
	return string(b[:min(len(b), 386)])
}
