package licenses

import (
	"context"
	"errors"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Rules a bundle's licence is refused by, besides the relay's and the token's (spec 0021).
const (
	RuleOff      = "off"             // licensing is off at the site
	RuleNoTarget = "no_target"       // no bundle-mode upstream of the source took the bundle
	RuleHolder   = "holder_mismatch" // not of the holder the plan named
)

// Exportable lists the licences of a tenant a bundle may carry (spec 0021), by name: those in
// force, whose key the server does not refuse, and of holder when one is named. Off, none.
func (s *Service) Exportable(ctx context.Context, tenantID, holder string) ([]store.License, error) {
	if !s.Enabled {
		return nil, ErrOff
	}
	ls, err := s.Store.ListLicenses(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	now := s.Store.Now()
	out := ls[:0]
	for _, l := range ls {
		t, err := license.Parse(l.Token)
		if err != nil || s.Refuse[l.Key] || holder != "" && l.HolderID != holder {
			continue
		}
		if st := t.StatusAt(now); st == license.Valid || st == license.Grace {
			out = append(out, l)
		}
	}
	return out, nil
}

// BundleLicense is a licence a bundle carries for a source, with the bundle-mode upstreams of that
// source the import fills, in the import's order.
type BundleLicense struct {
	Name, Token string
	Upstreams   []store.Upstream
}

// ApplyBundle stores the licences a bundle carries (spec 0021), in one transaction. Each goes to
// the first of its upstreams that supplies its name, is verified with that upstream's keys and, when
// holder is named, must be of that holder; spec 0012's relay rules apply, except that a bundle's
// licence never replaces an administrator's import. Nothing is removed: a bundle may be partial. A
// dry run stores nothing and counts. The result's Error is a failure that stored nothing.
func (s *Service) ApplyBundle(ctx context.Context, t store.Tenant, actor, holder string, ls []BundleLicense, dryRun bool) Result {
	res := Result{Counts: map[string]int{"received": len(ls)}}
	type item struct {
		BundleLicense
		tok *license.Token
	}
	var ok []item
	for _, bl := range ls {
		if !s.Enabled {
			res.refuse(bl.Name, "", RuleOff, ErrOff.Error())
			continue
		}
		tok, err := s.parse(bl.Token)
		if err != nil {
			rule := "not_in_force"
			var le *license.Error
			if errors.As(err, &le) {
				rule = le.Rule
			}
			id := ""
			if p, perr := license.Parse(bl.Token); perr == nil {
				id = p.ID
			}
			res.refuse(bl.Name, id, rule, err.Error())
			continue
		}
		switch {
		case tok.Extension != bl.Name:
			res.refuse(bl.Name, tok.ID, license.RuleExtMismatch, "the licence is of "+tok.Extension+", carried as "+bl.Name)
		case holder != "" && tok.HolderID != holder:
			res.refuse(bl.Name, tok.ID, RuleHolder, "the licence's holder is "+tok.HolderID+", the plan expects "+holder)
		case len(bl.Upstreams) == 0:
			res.refuse(bl.Name, tok.ID, RuleNoTarget, "no bundle-mode upstream of the source took this bundle")
		default:
			ok = append(ok, item{bl, tok})
		}
	}
	if len(ok) == 0 {
		return res
	}
	// the transaction may run again: its counts reach res only once it holds
	var counts map[string]int
	var refused []Refusal
	err := s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		counts, refused = map[string]int{}, nil
		for _, it := range ok {
			name := it.Name
			// the first upstream, as it is now, that supplies the name
			var u *store.Upstream
			for _, cand := range it.Upstreams {
				cur, err := tx.GetUpstreamByID(ctx, cand.ID)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				if cur.Mode != store.ModeBundle || cur.State != store.UpstreamActive {
					continue
				}
				supplied, err := tx.SuppliedBy(ctx, cur, name)
				if err != nil {
					return err
				}
				if supplied {
					u = &cur
					break
				}
			}
			if u == nil {
				refused = append(refused, Refusal{Name: name, ID: it.tok.ID, Rule: "not_supplied",
					Reason: "no bundle-mode upstream of the source supplies a release of " + name + " here"})
				continue
			}
			keys, err := tx.LiveLicenseKeys(ctx, t.ID, name, store.KeySourceUpstream, u.ID)
			if err != nil {
				return err
			}
			if err := s.verify(it.tok, keys, " from upstream "+u.Name); err != nil {
				rule := license.RuleKeyUntrusted
				var le *license.Error
				if errors.As(err, &le) {
					rule = le.Rule
				}
				refused = append(refused, Refusal{Name: name, ID: it.tok.ID, Rule: rule, Reason: err.Error()})
				continue
			}
			l := record(it.tok, t.ID, it.Token, actor)
			l.Source, l.UpstreamID = store.LicenseUpstream, u.ID
			old, err := tx.GetLicense(ctx, t.ID, name)
			switch {
			case errors.Is(err, store.ErrNotFound):
				old.Version = 0
			case err != nil:
				return err
			case old.Token == it.Token:
				counts["unchanged"]++
				continue
			case old.Source == store.LicenseImport:
				refused = append(refused, Refusal{Name: name, ID: it.tok.ID, Rule: "superseded",
					Reason: "the licence an administrator imported for " + name + " stays: a bundle's never replaces it"})
				continue
			case replaces(old, l, s.Store.Now().Unix(), s.deadKey(ctx, tx, old)):
				l.Visibility = old.Visibility
			default:
				refused = append(refused, Refusal{Name: name, ID: it.tok.ID, Rule: "superseded",
					Reason: "the licence stored for " + name + " (" + old.Source + ") stays: it is newer or lasts longer"})
				continue
			}
			if !dryRun {
				if err := tx.PutLicense(ctx, &l, old.Version); err != nil {
					return err
				}
			}
			if old.Version == 0 {
				counts["stored"]++
			} else {
				counts["replaced"]++
			}
		}
		return nil
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for k, v := range counts {
		res.Counts[k] += v
	}
	for _, r := range refused {
		res.refuse(r.Name, r.ID, r.Rule, r.Reason)
	}
	return res
}
