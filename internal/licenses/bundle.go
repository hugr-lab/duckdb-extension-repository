package licenses

import (
	"context"
	"errors"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Rules a bundle's licence is refused by, besides the relay's and the token's (spec 0021).
const (
	RuleOff      = "off"             // licensing is off at the site
	RuleNoTarget = "no_target"       // no bundle-mode upstream of the source took the bundle
	RuleHolder   = "holder_mismatch" // not of the holder the plan named, or the tenant expects (spec 0028)
)

// holderRefusal is the refusal of a licence of another holder than its tenant expects (spec
// 0028), read in the transaction that would store it; nil when the tenant takes it.
func holderRefusal(ctx context.Context, tx *store.Tx, tenantID string, tok *license.Token) (*Refusal, error) {
	err := tx.CheckLicenseHolder(ctx, tenantID, tok.HolderID)
	var hm *store.HolderMismatch
	switch {
	case err == nil:
		return nil, nil
	case errors.As(err, &hm):
		return &Refusal{Name: tok.Extension, ID: tok.ID, Rule: RuleHolder, Reason: hm.Error()}, nil
	}
	return nil, err
}

// Exportable lists the licences of a package type of a tenant a bundle may carry (spec 0021; spec
// 0034: a bundle's sources are of one type each), by name: those in force, whose key the server
// does not refuse, of holder when one is named, and whose key is live for its entry (spec 0035).
// Off, none.
func (s *Service) Exportable(ctx context.Context, tenantID, packageType, holder string) ([]store.License, error) {
	if !s.Enabled {
		return nil, ErrOff
	}
	ls, err := s.Store.ListLicenses(ctx, tenantID, packageType)
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
		if st := t.StatusAt(now); st != license.Valid && st != license.Grace {
			continue
		}
		// spec 0035: only a licence its entry serves travels: its key live for the entry (a removed
		// source's licence, or one whose key no build of the entry declares now, is served nowhere)
		live := false
		if err := s.Store.InTxLocks(ctx, nil, func(tx *store.Tx) error {
			keys, err := tx.LiveLicenseKeys(ctx, tenantID, packageType, l.Name, l.Entry)
			live = slices.ContainsFunc(keys, func(k store.LicenseKey) bool { return k.Kid == l.Key })
			return err
		}); err != nil {
			return nil, err
		}
		if live {
			out = append(out, l)
		}
	}
	return out, nil
}

// BundleLicense is a licence a bundle carries, read once however many of its sources carry it, with
// the bundle-mode upstreams of those sources the import fills, in the import's order.
type BundleLicense struct {
	Name, Token string
	Upstreams   []store.Upstream
}

// ApplyBundle stores the licences a bundle carries (spec 0021), in one transaction. Each goes into
// the entry of every one of its upstreams that supplies its name (spec 0035), verified there with
// that entry's live keys and, when holder is named, of that holder; spec 0012's relay rules apply
// per entry, except that a bundle's licence never replaces an administrator's import. The counts
// (stored, replaced, unchanged, refused) are per target. Nothing is removed: a bundle may be
// partial. A dry run stores nothing and counts. The result's Error is a failure that stored nothing.
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
	lb, err := s.labels(ctx, t.ID)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	// the transaction may run again: its counts reach res only once it holds
	var counts map[string]int
	var per map[string]map[string]int // by target
	var refused []Refusal
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		tx.SetWarningAct("bundle.import") // spec 0035 phase 2: a licence stored into an entry served publicly
		counts, per, refused = map[string]int{}, map[string]map[string]int{}, nil
		for _, it := range ok {
			name := it.Name
			// spec 0035: every upstream of the licence's sources, as it is now, that supplies the name
			// takes it into its own entry, each checked as a relayed licence is
			var targets []store.Upstream
			var types []string
			for _, cand := range it.Upstreams {
				if slices.ContainsFunc(targets, func(u store.Upstream) bool { return u.ID == cand.ID }) {
					continue
				}
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
				home, err := tx.ChannelByID(ctx, cur.ChannelID) // spec 0034: the upstream's type, its home channel's
				if err != nil {
					return err
				}
				supplied, err := tx.SuppliedBy(ctx, cur, home.PackageType, name)
				if err != nil {
					return err
				}
				if supplied {
					targets, types = append(targets, cur), append(types, home.PackageType)
				}
			}
			if len(targets) == 0 {
				refused = append(refused, Refusal{Name: name, ID: it.tok.ID, Rule: "not_supplied",
					Reason: "no bundle-mode upstream of the source supplies a release of " + name + " here"})
				continue
			}
			for i, u := range targets {
				typ, src := types[i], store.UpstreamSource(u.ID)
				// the counts are per target: in all, and the target's own
				if per[u.Name] == nil {
					per[u.Name] = map[string]int{}
				}
				count := func(k string) { counts[k]++; per[u.Name][k]++ }
				refuse := func(r Refusal) { refused = append(refused, r); per[u.Name]["refused"]++ }
				// the live keys of the upstream's own entry verify it (spec 0021; spec 0035)
				if err := s.verifyIn(ctx, tx, t.ID, typ, src, it.tok, lb.of(src)); err != nil {
					if r := refusal(err, it.tok); r != nil {
						refuse(*r)
						continue
					}
					return err
				}
				// spec 0028: the tenant's expected holder, besides the plan's (checked by both, a dry run too)
				if r, err := holderRefusal(ctx, tx, t.ID, it.tok); err != nil {
					return err
				} else if r != nil {
					refuse(*r)
					continue
				}
				l := record(it.tok, t.ID, typ, it.Token, actor)
				l.Entry, l.Source, l.UpstreamID = src, store.LicenseUpstream, u.ID
				old, err := tx.GetLicense(ctx, t.ID, typ, name, src)
				switch {
				case errors.Is(err, store.ErrNotFound):
					old = store.License{}
				case err != nil:
					return err
				case old.Token == it.Token:
					count("unchanged")
					continue
				}
				// spec 0037: never older than a licence the tenant holds (a bundle imported again);
				// before the refusal of a licence over an import, which an older one would also meet
				if err := s.older(ctx, tx, lb, l); err != nil {
					if r := refusal(err, it.tok); r != nil {
						refuse(*r)
						continue
					}
					return err
				}
				switch {
				case old.Version == 0:
				case old.Source == store.LicenseImport:
					refuse(Refusal{Name: name, ID: it.tok.ID, Rule: "superseded",
						Reason: "the licence an administrator imported for " + name + " from " + lb.of(src) + " stays: a bundle's never replaces it"})
					continue
				case replaces(old, l, s.Store.Now().Unix(), s.deadKey(ctx, tx, old)):
					l.Visibility = old.Visibility
				default:
					refuse(Refusal{Name: name, ID: it.tok.ID, Rule: "superseded",
						Reason: "the licence stored for " + name + " from " + lb.of(src) + " (" + old.Source + ") stays: it is newer or lasts longer"})
					continue
				}
				if !dryRun {
					if err := tx.PutLicense(ctx, &l, old.Version); err != nil {
						return err
					}
				}
				if old.Version == 0 {
					count("stored")
				} else {
					count("replaced")
				}
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
	if len(per) > 0 {
		res.Targets = per
	}
	return res
}
