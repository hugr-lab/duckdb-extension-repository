package release_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// placing is a tenant with a publisher (ci) publishing into staging, and a pass over every rule.
type placing struct {
	*env
	pubs *release.Service
	pub  authz.Actor
	ci   store.Publisher
}

func newPlacing(t *testing.T, en *env) placing {
	t.Helper()
	en.staging(t)
	prod, err := en.st.GetChannel(ctx, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	ci := store.Publisher{TenantID: prod.TenantID, Name: "ci"}
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &ci) })
	})
	pubs := *en.rel
	pubs.Authz = allowAll{}
	return placing{env: en, pubs: &pubs, ci: ci,
		pub: authz.Actor{Kind: authz.ActorPublisher, ID: "acme/ci", Tenant: "acme", Principals: auth.Principals{auth.PublisherKey(ci.ID): true}}}
}

func (p placing) publish(t *testing.T, m extfile.Metadata) {
	t.Helper()
	entry := "tresor_duckdb_cpp_init"
	if m.ABI == extfile.ABICStruct {
		entry = "tresor_init_c_api"
	}
	if _, _, err := p.pubs.Add(ctx, p.pub, "acme", "staging", bytes.NewReader(built(t, m, entry)),
		publish("tresor", m.ExtensionVersion, m.Platform)); err != nil {
		t.Fatalf("publish %s: %v", m.ExtensionVersion, err)
	}
}

// pass runs the placement pass over the due rules, or every rule.
func (p placing) pass(t *testing.T, sweep bool) {
	t.Helper()
	(&release.Placer{Service: p.rel, Holder: "test", Log: slog.New(slog.DiscardHandler)}).Once(ctx, sweep)
}

// prod lists prod's releases of tresor as version@slot:state, oldest first.
func (p placing) prod(t *testing.T) string {
	t.Helper()
	ch, err := p.st.GetChannel(ctx, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := p.st.ListReleases(ctx, ch.ID, "tresor")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.ExtVersion+"@"+r.Slot+":"+r.State)
	}
	slices.Reverse(out)
	return strings.Join(out, " ")
}

// Spec 0033: an auto rule places, per cell, each newer version of its source that the channel
// serves; a version yanked there is not placed again, nor one older than what the cell had.
func TestPlaceAuto(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		p.publish(t, cpp("1.0"))
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "auto"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, false)
		if got := p.prod(t); got != "1.0@v2.0.0:active" {
			t.Fatalf("after the first pass: %q", got)
		}
		p.publish(t, cpp("1.1"))
		p.pass(t, false)
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.1@v2.0.0:active" {
			t.Fatalf("a newer version: %q", got)
		}
		ch, _ := p.st.GetChannel(ctx, "acme", "prod")
		rs, _ := p.st.ListReleases(ctx, ch.ID, "tresor")
		if _, err := p.rel.Apply(ctx, admin, "acme", "prod", "tresor", rs[0].ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		p.publish(t, cpp("1.0.5")) // older than 1.1, which the cell had
		p.pass(t, true)
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.1@v2.0.0:yanked" {
			t.Fatalf("yanked or older placed: %q", got)
		}
		// a C API build newer than everything serves the DuckDB version; an older cpp one after it does not
		p.publish(t, capi("2.0", "v1.2.0"))
		p.publish(t, cpp("1.5"))
		p.pass(t, false)
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.1@v2.0.0:yanked 2.0@capi:1:active" {
			t.Fatalf("a C API build, then an older cpp one: %q", got)
		}
		// the rule's view: its mode, the version held and the newest in the catalogue
		v, err := p.rel.Source(ctx, admin, "acme", "prod", "tresor")
		if err != nil || v.Mode != "auto" || v.Current != "2.0" || v.Newest != "2.0" {
			t.Fatalf("the view: %+v %v", v, err)
		}
		// a pass again places nothing more
		p.pass(t, false)
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.1@v2.0.0:yanked 2.0@capi:1:active" {
			t.Fatalf("after a pass again: %q", got)
		}
	})
}

// Spec 0033 (the owner's choice): a manual rule places no new version, but completes the versions the
// channel holds: a build of one for a DuckDB version the channel serves is placed, when the channel
// starts serving that version too; not current where a newer version serves the cell, and private
// where the version is private in the channel.
func TestPlaceManualCompletes(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		must(t, func() error {
			_, _, err := p.ten.AddVersion(ctx, admin, "v2.1.0", "release", []string{"v1.5.6", "v2.0.0"}, tenants.CoreExtensions{Names: []string{"json"}})
			return err
		})
		must(t, func() error {
			_, err := p.ten.SetChannelVersions(ctx, admin, "acme", "staging", []string{"v2.1.0"}, nil)
			return err
		})
		p.publish(t, cpp("1.0"))
		p.publish(t, cpp("1.1"))
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "manual"); err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"1.0", "1.1"} {
			if _, _, err := p.rel.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: v}); err != nil {
				t.Fatal(err)
			}
		}
		ch, _ := p.st.GetChannel(ctx, "acme", "prod")
		rs, _ := p.st.ListReleases(ctx, ch.ID, "tresor")
		for _, r := range rs {
			if r.ExtVersion == "1.0" {
				if _, err := p.rel.Apply(ctx, admin, "acme", "prod", "tresor", r.ID, release.SetPrivate, 0); err != nil {
					t.Fatal(err)
				}
			}
		}
		p.publish(t, cpp("2.0"))
		on21 := func(v string) extfile.Metadata {
			m := cpp(v)
			m.DuckDBVersion = "v2.1.0"
			return m
		}
		p.publish(t, on21("1.1"))
		p.pass(t, false)
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.1@v2.0.0:active" {
			t.Fatalf("placed for a DuckDB version prod does not serve: %q", got)
		}
		must(t, func() error {
			_, err := p.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.1.0"}, nil)
			return err
		})
		p.pass(t, false) // the version added makes the rule due again
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.1@v2.0.0:active 1.1@v2.1.0:active" {
			t.Fatalf("after v2.1.0: %q", got)
		}
		p.publish(t, on21("1.0")) // older than the cell's 1.1: placed, not current
		p.pass(t, false)
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.1@v2.0.0:active 1.1@v2.1.0:active 1.0@v2.1.0:active" {
			t.Fatalf("after 1.0 for v2.1.0: %q", got)
		}
		rs, _ = p.st.ListReleases(ctx, ch.ID, "tresor")
		for _, r := range rs {
			if r.Slot != "v2.1.0" {
				continue
			}
			if r.ExtVersion == "1.0" && (r.Seq != 0 || r.Visibility != store.Private) || r.ExtVersion == "1.1" && r.Seq == 0 {
				t.Fatalf("a completion: %s seq %d %s", r.ExtVersion, r.Seq, r.Visibility)
			}
		}
	})
}

// Spec 0033: the tenant's own source is everyone who may publish in it: the pass never places from
// it, so a grant on staging never reaches prod.
func TestPlaceNeverTenant(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		must(t, func() error {
			_, _, err := p.ten.AddVersion(ctx, admin, "v2.1.0", "release", []string{"v1.5.6", "v2.0.0"}, tenants.CoreExtensions{Names: []string{"json"}})
			return err
		})
		for _, ch := range []string{"staging", "prod"} {
			must(t, func() error {
				_, err := p.ten.SetChannelVersions(ctx, admin, "acme", ch, []string{"v2.1.0"}, nil)
				return err
			})
		}
		if _, _, err := en.addTo(t, "prod", built(t, cpp("1.0"), "tresor_duckdb_cpp_init"), release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "tenant", "manual"); err != nil {
			t.Fatal(err)
		}
		m := cpp("1.0")
		m.DuckDBVersion = "v2.1.0"
		if _, err := p.rel.SetSource(ctx, admin, "acme", "staging", "tresor", "tenant", ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := en.addTo(t, "staging", built(t, m, "tresor_duckdb_cpp_init", "other"), release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		p.pass(t, true)
		if got := p.prod(t); got != "1.0@v2.0.0:active" {
			t.Fatalf("the tenant's staging build reached prod: %q", got)
		}
	})
}

// promoter allows promote, and publish in staging: what promoting from staging takes.
type promoter struct{}

func (promoter) Allow(_ context.Context, _ authz.Actor, v authz.Verb, r authz.Resource) error {
	if v == authz.VerbPromote || v == authz.VerbPublish && r.Channel == "staging" {
		return nil
	}
	return authz.ErrDenied
}

// Spec 0033: a placement by hand takes promote; a channel administrator places any build of the
// catalogue and writes a name's rule (manual); anyone else only what a promotion would take (a
// release active where it may publish), and learns nothing else (404); never another source's name.
func TestPlaceByHand(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		p.publish(t, cpp("1.0"))
		promoting := *p.rel
		promoting.Authz = promoter{}
		if _, _, err := promoting.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: "1.0"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a promoter where no rule is: %v", err)
		}
		if _, _, err := promoting.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "upstream:nope", Version: "1.0"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a promoter naming no source: %v", err)
		}
		if _, _, err := p.rel.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: "9.9"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a version the catalogue lacks: %v", err)
		}
		rs, existed, err := p.rel.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: "1.0", Private: true})
		if err != nil || existed || len(rs) != 1 || rs[0].Origin != store.OriginPlacement || rs[0].Visibility != store.Private {
			t.Fatalf("placed: %+v %v %v", rs, existed, err)
		}
		v, err := p.rel.Source(ctx, admin, "acme", "prod", "tresor")
		if err != nil || v.Mode != "manual" || v.Source != "publisher:ci" {
			t.Fatalf("the rule the placement wrote: %+v %v", v, err)
		}
		// the promoter may place now the rule exists: 1.0 is active in staging, where it may publish
		if _, existed, err := promoting.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: "1.0",
			Private: true}); err != nil || !existed {
			t.Fatalf("placed again: %v %v", existed, err)
		}
		if _, _, err := en.addTo(t, "staging", built(t, cpp("3.0"), "tresor_duckdb_cpp_init"), release.AddOptions{Name: "tresor"}); !errors.Is(err, release.ErrOtherSource) {
			t.Fatalf("the tenant's in staging: %v", err)
		}
		if _, _, err := p.rel.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "tenant", Version: "1.0"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the tenant's source has no build: %v", err)
		}
		// the tenant's source is never automatic
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "tenant", "auto"); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an auto rule on the tenant: %v", err)
		}
		// the catalogue: the entry, its versions, its builds and where they are released
		es, err := p.rel.Catalogue(ctx, admin, "acme", "", "tresor")
		if err != nil || len(es) != 1 || es[0].Source != "publisher:ci" || strings.Join(es[0].Versions, " ") != "1.0" ||
			es[0].Rules["prod"] != "manual" || es[0].Rules["staging"] != "" {
			t.Fatalf("the catalogue: %+v %v", es, err)
		}
		vs, err := p.rel.CatalogueVersions(ctx, admin, "acme", "publisher:ci", "tresor")
		if err != nil || len(vs) != 1 || len(vs[0].Builds) != 1 || len(vs[0].Builds[0].Placed) != 2 {
			t.Fatalf("the entry: %+v %v", vs, err)
		}
	})
}

// authzOf is a publisher's actor, as its signed request makes it.
func authzOf(p store.Publisher) authz.Actor {
	return authz.Actor{Kind: authz.ActorPublisher, ID: "acme/" + p.Name, Tenant: "acme", Principals: auth.Principals{auth.PublisherKey(p.ID): true}}
}
