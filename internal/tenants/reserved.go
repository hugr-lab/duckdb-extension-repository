package tenants

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0030: the server's reserved names, as an administrator sees and changes them. A version's
// core extensions come with the version (AddVersion), an upstream's names with its intake; these
// are the names an administrator adds, and removes again.

// ListReserved lists the reserved names of a package type ("": the default) with their sources (one
// name, or all when name is ""): server administrators and distribution admins read them.
func (s *Service) ListReserved(ctx context.Context, a authz.Actor, packageType, name string) ([]store.ReservedName, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbDistribute, authz.Server); err != nil {
		return nil, err
	}
	packageType, err := parseType(packageType)
	if err != nil {
		return nil, err
	}
	return s.Store.ListReservedNames(ctx, packageType, name)
}

// parseType reads a package type a caller names (spec 0034): "" is the default, an unknown type
// invalid.
func parseType(t string) (string, error) {
	t, err := pkgtype.Parse(t)
	if err != nil {
		return "", fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	return t, nil
}

// typedFields adds an event's package_type when it is not the default (spec 0034).
func typedFields(f map[string]any, packageType string) map[string]any {
	if packageType != pkgtype.Default {
		f["package_type"] = packageType
	}
	return f
}

// Reserved is what an administrator's reservation did: the names that were not reserved as kind
// before, and the tenants that keep releases under them (by name).
type Reserved struct {
	Added   []string
	Tenants map[string][]string
}

// Reserve reserves names of a package type ("": the default) as core or community, with the source
// admin:<actor>. A name with releases that are not yanked in some tenant is refused unless forced:
// the tenants keep those releases (spec 0008), and only grants naming the extension reach the name
// from then on.
func (s *Service) Reserve(ctx context.Context, a authz.Actor, packageType string, names []string, kind string, force bool) (Reserved, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return Reserved{}, err
	}
	packageType, err := parseType(packageType)
	if err != nil {
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
	held, err := s.held(ctx, packageType, names, kind)
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
			changed, err := tx.ReserveName(ctx, packageType, n, kind, src)
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
			res.Added = append(res.Added, n)
			data := typedFields(map[string]any{"kind": kind, "names": []string{n}, "source": src}, packageType)
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
func (s *Service) held(ctx context.Context, packageType string, names []string, kind string) (map[string][]string, error) {
	kinds, _, err := s.Store.ReservedNames(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, n := range names {
		if k := kinds[pkgtype.Key{Type: packageType, Name: n}]; k == kind || k == store.ReservedCore {
			continue
		}
		ts, err := s.Store.TenantsWithReleases(ctx, packageType, n)
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

// refuseHeld is heldRefusal for a reservation that answers no tenants (a version's core extensions:
// DuckDB extensions).
func (s *Service) refuseHeld(ctx context.Context, names []string, kind string, force bool) error {
	held, err := s.held(ctx, pkgtype.DuckDBExtension, names, kind)
	if err != nil {
		return err
	}
	return heldRefusal(held, force)
}

// Unreserve removes a name of a package type ("": the default) only administrators reserved; a
// name DuckDB brought (a version's, an upstream's, the seed's) stays.
func (s *Service) Unreserve(ctx context.Context, a authz.Actor, packageType, name string) error {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return err
	}
	packageType, err := parseType(packageType)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		sources, err := tx.RemoveReservedName(ctx, packageType, name)
		if errors.Is(err, store.ErrConflict) {
			return &Refusal{Reason: strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": ")}
		}
		if err != nil {
			return err
		}
		return tx.Event(ctx, "", a.String(), "reserved.remove", "reserved:"+name,
			typedFields(map[string]any{"name": name, "sources": sources}, packageType))
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
