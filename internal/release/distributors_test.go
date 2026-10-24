package release_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033 phase B part 1: a distributor's publisher publishes into the catalogue without grants,
// any name not reserved server-wide nor in the tenant for another holder, and never on a channel's
// route; a follow of it takes its names (a name reserved for its distributor included, one reserved
// for another holder skipped) though it holds no grant; a withdrawn version is served nowhere,
// placed by no rule, follow or hand, and the next version is placed as usual.
func TestDistributorFollowAndWithdraw(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.rel.Authz = publishes{} // no publisher holds a grant
		p := newPlacing(t, en)
		dist := store.Publisher{TenantID: p.ci.TenantID, Name: "enterest-ci"}
		reserve := func(name, holder string) {
			must(t, func() error {
				return p.st.InTx(ctx, "", func(tx *store.Tx) error {
					_, _, err := tx.ReserveTenantName(ctx, &store.TenantReservedName{TenantID: dist.TenantID, Name: name, Holder: holder, AddedBy: "os:1:t"})
					return err
				})
			})
		}
		must(t, func() error {
			return p.st.InTx(ctx, "", func(tx *store.Tx) error {
				if err := tx.InsertPublisher(ctx, &dist); err != nil {
					return err
				}
				_, err := tx.SetPublisherDistributor(ctx, dist.TenantID, dist.Name, "enterest")
				return err
			})
		})
		reserve("mine", "enterest")
		reserve("theirs", "someone-else")
		reserve("later", "enterest")
		pub := authzOf(dist)
		land := func(name, version string) error {
			_, _, err := p.rel.AddToCatalogue(ctx, pub, "acme", bytes.NewReader(built(t, cpp(version), name+"_duckdb_cpp_init")),
				publish(name, version, "linux_amd64"))
			return err
		}
		for _, n := range []string{"geo", "mine", "later"} {
			if err := land(n, "1.0"); err != nil {
				t.Fatalf("%s: %v", n, err)
			}
		}
		for _, n := range []string{"theirs", "json"} { // reserved for another holder; DuckDB's core name
			if err := land(n, "1.0"); !errors.Is(err, authz.ErrDenied) {
				t.Fatalf("%s: %v", n, err)
			}
		}
		// never on a channel's route, whatever its grants would say
		if _, _, err := p.pubs.Add(ctx, pub, "acme", "prod", bytes.NewReader(built(t, cpp("0.9"), "geo_duckdb_cpp_init")),
			publish("geo", "0.9", "linux_amd64")); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("a distributor's publisher on a channel's route: %v", err)
		}
		// the name reserved for the distributor changes holder before the follow looks
		reserve("later", "someone-else")
		if _, err := p.rel.SetFollow(ctx, admin, "acme", "prod", "publisher:enterest-ci"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, false)
		if got := p.names(t); got != "geo@1.0 mine@1.0" {
			t.Fatalf("the follow placed: %q", got)
		}
		v, err := p.rel.Follow(ctx, admin, "acme", "prod", "publisher:enterest-ci")
		if err != nil || len(v.Skips) != 1 || v.Skips["later"] != "a name reserved in the tenant: a rule naming it takes it" {
			t.Fatalf("the follow's skips: %+v %v", v, err)
		}
		// withdrawn: a distribution admin's or a server administrator's only
		if _, err := p.rel.Withdraw(ctx, p.pub, "acme", "", "publisher:enterest-ci", "geo", "1.0", "CVE"); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("withdrawn by a publisher: %v", err)
		}
		if _, err := p.rel.Withdraw(ctx, admin, "acme", "", "publisher:ci", "geo", "1.0", "CVE"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the tenant's own publisher's: %v", err)
		}
		w, err := p.rel.Withdraw(ctx, admin, "acme", "", "publisher:enterest-ci", "geo", "1.0", "CVE-2026-0001")
		if err != nil || w.Already || len(w.Builds) != 1 || w.Tenant != "acme" {
			t.Fatalf("withdrawn: %+v %v", w, err)
		}
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
				t.Fatalf("served after its withdrawal: %+v", r)
			}
		}
		// placed by no one; the next version as usual
		if _, _, err := p.rel.Place(ctx, admin, "acme", "staging", "geo", release.PlaceOptions{Source: "publisher:enterest-ci", Version: "1.0",
			Early: true}); !errors.Is(err, store.ErrWithdrawn) {
			t.Fatalf("placed by hand: %v", err)
		}
		if err := land("geo", "1.0"); !errors.Is(err, store.ErrWithdrawn) {
			t.Fatalf("the body again: %v", err)
		}
		if err := land("geo", "1.1"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, true)
		if got := p.names(t); got != "geo@1.0 mine@1.0 geo@1.1" {
			t.Fatalf("after the withdrawal: %q", got)
		}
		s, err := p.rel.Source(ctx, admin, "acme", "prod", "geo")
		if err != nil || s.Current != "1.1" || s.Newest != "1.1" {
			t.Fatalf("the rule's versions: %+v %v", s, err)
		}
	})
}
