package release_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// distributorPlacing is newPlacing with a fresh distributor's publisher, enterest-ci, and a function
// publishing a name's version for a platform into the catalogue as it.
func distributorPlacing(t *testing.T, en *env) (placing, func(name, version, platform string) error) {
	t.Helper()
	en.rel.Authz = publishes{}
	p := newPlacing(t, en)
	dist := store.Publisher{TenantID: p.ci.TenantID, Name: "enterest-ci", CreatedBy: "os:1:t"}
	must(t, func() error {
		return p.st.InTx(ctx, "", func(tx *store.Tx) error {
			if err := tx.InsertPublisher(ctx, &dist); err != nil {
				return err
			}
			_, err := tx.SetPublisherDistributor(ctx, dist.TenantID, dist.Name, "enterest")
			return err
		})
	})
	pub := authzOf(dist)
	return p, func(name, version, platform string) error {
		m := cpp(version)
		m.Platform = platform
		_, _, err := p.rel.AddToCatalogue(ctx, pub, "acme", bytes.NewReader(built(t, m, name+"_duckdb_cpp_init")),
			publish(name, version, platform))
		return err
	}
}

// Spec 0033 phase B (review): a withdrawal is the version's, for good: a build of it for another
// platform is refused afterwards (a manual rule completing the version would have placed it), a
// version withdrawn before it arrived never arrives, and a withdrawn release holds its version for no
// one.
func TestWithdrawnVersionFinal(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p, land := distributorPlacing(t, en)
		must(t, func() error { return land("geo", "1.0", "linux_amd64") })
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "geo", "publisher:enterest-ci", store.RuleManual); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.rel.Place(ctx, admin, "acme", "prod", "geo", release.PlaceOptions{Source: "publisher:enterest-ci", Version: "1.0"}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.rel.Withdraw(ctx, admin, "acme", "", "publisher:enterest-ci", "geo", "1.0", "CVE"); err != nil {
			t.Fatal(err)
		}
		if err := land("geo", "1.0", "linux_amd64_musl"); !errors.Is(err, store.ErrWithdrawn) {
			t.Fatalf("another platform of a withdrawn version: %v", err)
		}
		p.pass(t, true)
		prod, err := p.st.GetChannel(ctx, "acme", "prod")
		if err != nil {
			t.Fatal(err)
		}
		snap, err := (&release.Snapshots{Store: p.st}).Get(ctx, prod)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range snap.Releases {
			if r.Name == "geo" {
				t.Fatalf("a withdrawn version served again: %+v", r)
			}
		}
		if w, err := p.rel.Withdraw(ctx, admin, "acme", "", "publisher:enterest-ci", "geo", "1.0", "CVE"); err != nil || !w.Already {
			t.Fatalf("withdrawn again: %+v %v", w, err)
		}
		rels, err := p.st.ListReleases(ctx, prod.ID, "geo")
		if err != nil || len(rels) != 1 || !rels[0].Withdrawn {
			t.Fatalf("the withdrawn release: %+v %v", rels, err)
		}
		if held := release.ServedHeld(rels, map[string][]store.CAPI{"v2.0.0": nil}); held["1.0"] {
			t.Fatal("a withdrawn release holds its version")
		}
		// withdrawn before it arrived: no entry, the answer says so, and it never arrives
		w, err := p.rel.Withdraw(ctx, admin, "acme", "", "publisher:enterest-ci", "geo", "4.0", "pulled before release")
		if err != nil || w.Already || len(w.Builds) != 0 {
			t.Fatalf("a version not arrived: %+v %v", w, err)
		}
		if err := land("geo", "4.0", "linux_amd64"); !errors.Is(err, store.ErrWithdrawn) {
			t.Fatalf("a version withdrawn before it arrived: %v", err)
		}
		if _, err := p.rel.Withdraw(ctx, admin, "acme", "", "publisher:nope", "geo", "4.0", "x"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("an unknown source: %v", err)
		}
	})
}

// Spec 0033 phase B (review; the owner's decision, 2026-10-24): after a withdrawal an auto rule or a
// follow never goes back down to an older version by itself; a newer one is placed as usual.
func TestWithdrawNoDowngrade(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p, land := distributorPlacing(t, en)
		must(t, func() error { return land("geo", "1.0", "linux_amd64") })
		must(t, func() error { return land("geo", "2.0", "linux_amd64") })
		if _, err := p.rel.SetFollow(ctx, admin, "acme", "prod", "publisher:enterest-ci"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, true)
		if got := p.names(t); got != "geo@2.0" {
			t.Fatalf("followed: %q", got)
		}
		if _, err := p.rel.Withdraw(ctx, admin, "acme", "", "publisher:enterest-ci", "geo", "2.0", "bad"); err != nil {
			t.Fatal(err)
		}
		must(t, func() error { return land("geo", "1.5", "linux_amd64") })
		p.pass(t, true)
		if got := p.names(t); got != "geo@2.0" {
			t.Fatalf("an older version placed after the withdrawal: %q", got)
		}
		must(t, func() error { return land("geo", "3.0", "linux_amd64") })
		p.pass(t, true)
		if got := p.names(t); got != "geo@2.0 geo@3.0" {
			t.Fatalf("a newer version: %q", got)
		}
	})
}

// Spec 0033 phase B (review): a promotion of a withdrawn entry's release into a channel holding the
// same build already is refused, not answered as made.
func TestWithdrawnPromotion(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p, land := distributorPlacing(t, en)
		must(t, func() error { return land("geo", "1.0", "linux_amd64") })
		for _, ch := range []string{"staging", "prod"} {
			if _, _, err := p.rel.Place(ctx, admin, "acme", ch, "geo", release.PlaceOptions{Source: "publisher:enterest-ci", Version: "1.0"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := p.rel.Promote(ctx, admin, "acme", "prod", "geo", release.PromoteOptions{From: "staging", Version: "1.0"}); err != nil {
			t.Fatalf("before the withdrawal: %v", err)
		}
		if _, err := p.rel.Withdraw(ctx, admin, "acme", "", "publisher:enterest-ci", "geo", "1.0", "CVE"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.rel.Promote(ctx, admin, "acme", "prod", "geo", release.PromoteOptions{From: "staging", Version: "1.0"}); !errors.Is(err, store.ErrWithdrawn) {
			t.Fatalf("a withdrawn release promoted: %v", err)
		}
	})
}

// Spec 0033 phase B (review): a distributor's publication checks its name again in the landing
// transaction: reserved server-wide or for another holder meanwhile, or the tenant suspended.
func TestDistributorMayNow(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p, _ := distributorPlacing(t, en)
		tid := p.ci.TenantID
		check := func(name string) error {
			return p.st.InTx(ctx, "", func(tx *store.Tx) error {
				return release.DistributorMayNow(ctx, tx, tid, "duckdb_extension", name, "enterest")
			})
		}
		must(t, func() error {
			return p.st.InTx(ctx, "", func(tx *store.Tx) error {
				for name, holder := range map[string]string{"mine": "enterest", "theirs": "someone-else"} {
					if _, _, err := tx.ReserveTenantName(ctx, &store.TenantReservedName{TenantID: tid, Name: name, Holder: holder, AddedBy: "os:1:t"}); err != nil {
						return err
					}
				}
				return nil
			})
		})
		for name, denied := range map[string]bool{"geo": false, "mine": false, "theirs": true, "json": true} {
			if err := check(name); denied && !errors.Is(err, authz.ErrDenied) || !denied && err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := p.ten.SetTenantState(ctx, admin, "acme", store.TenantSuspended, 0); err != nil {
			t.Fatal(err)
		}
		if err := check("geo"); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("a suspended tenant: %v", err)
		}
	})
}
