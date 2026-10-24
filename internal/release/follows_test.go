package release_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// land publishes a name's version into the catalogue only, as the publisher ci.
func (p placing) land(t *testing.T, name, version string) {
	t.Helper()
	if _, _, err := p.pubs.AddToCatalogue(ctx, p.pub, "acme", bytes.NewReader(built(t, cpp(version), name+"_duckdb_cpp_init")),
		publish(name, version, "linux_amd64")); err != nil {
		t.Fatalf("%s %s: %v", name, version, err)
	}
}

// names lists prod's releases as name@version, oldest first.
func (p placing) names(t *testing.T) string {
	t.Helper()
	ch, _ := p.st.GetChannel(ctx, "acme", "prod")
	rs, err := p.st.ListReleases(ctx, ch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(rs) - 1; i >= 0; i-- {
		out = append(out, rs[i].Name+"@"+rs[i].PackageVersion)
	}
	return strings.Join(out, " ")
}

// publishes allows administrators as authz.ServerAdmin does, and a publisher the names it holds
// publish on (in every channel).
type publishes map[string]bool

func (g publishes) Allow(ctx context.Context, a authz.Actor, v authz.Verb, r authz.Resource) error {
	if a.Kind == authz.ActorPublisher {
		if v == authz.VerbPublish && g[r.Package] {
			return nil
		}
		return authz.ErrDenied
	}
	return authz.ServerAdmin{}.Allow(ctx, a, v, r)
}

// Spec 0033: a channel following a source takes every name of it the channel has no rule for, each
// new version as it lands, a rule an arrival wrote for the source included; it skips a name the
// tenant uses from another source, one two follows offer, and one the publisher may not publish in
// the channel; once removed, it places nothing more, and its rules keep completing the versions they
// hold; a removed source's follows go with it.
func TestFollow(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.rel.Authz = publishes{"tresor": true, "geo": true, "shared": true, "arrived": true}
		p := newPlacing(t, en)
		other := store.Publisher{TenantID: p.ci.TenantID, Name: "other"}
		must(t, func() error {
			return p.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &other) })
		})
		if _, err := p.rel.SetFollow(ctx, admin, "acme", "prod", "tenant"); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("following the tenant: %v", err)
		}
		if _, err := p.rel.SetFollow(ctx, admin, "acme", "mirror", "publisher:ci"); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a passthrough channel following: %v", err)
		}
		for _, n := range []string{"tresor", "geo", "shared", "secret"} {
			p.land(t, n, "1.0")
		}
		// a publication of ci's straight into prod: its rule has no mode, the follow takes it over
		if _, _, err := p.pubs.Add(ctx, p.pub, "acme", "prod", bytes.NewReader(built(t, cpp("0.9"), "arrived_duckdb_cpp_init")),
			publish("arrived", "0.9", "linux_amd64")); err != nil {
			t.Fatal(err)
		}
		p.land(t, "arrived", "1.0")
		// the tenant serves geo from its own source in staging; the other publisher offers shared too
		if _, _, err := en.addTo(t, "staging", built(t, cpp("0.1"), "geo_duckdb_cpp_init"), release.AddOptions{Name: "geo"}); err != nil {
			t.Fatal(err)
		}
		for _, src := range []string{"publisher:ci", "publisher:other"} {
			if _, err := p.rel.SetFollow(ctx, admin, "acme", "prod", src); err != nil {
				t.Fatal(err)
			}
		}
		otherPub := *p.pubs
		if _, _, err := otherPub.AddToCatalogue(ctx, authzOf(other), "acme", bytes.NewReader(built(t, cpp("2.0"), "shared_duckdb_cpp_init", "x")),
			publish("shared", "2.0", "linux_amd64")); err != nil {
			t.Fatal(err)
		}
		p.pass(t, false)
		if got := p.names(t); got != "arrived@0.9 arrived@1.0 tresor@1.0" {
			t.Fatalf("after the first pass: %q", got)
		}
		v, err := p.rel.Follow(ctx, admin, "acme", "prod", "publisher:ci")
		if err != nil || len(v.Skips) != 3 || v.Skips["geo"] != "the tenant uses it from another source" ||
			!strings.HasPrefix(v.Skips["shared"], "contested") || !strings.HasPrefix(v.Skips["secret"], "the publisher may not publish it") {
			t.Fatalf("the follow's skips: %+v %v", v, err)
		}
		s, err := p.rel.Source(ctx, admin, "acme", "prod", "arrived")
		if err != nil || s.Mode != store.RuleFollow {
			t.Fatalf("the arrival's rule, taken over: %+v %v", s, err)
		}
		p.land(t, "tresor", "1.1")
		p.pass(t, false)
		if got := p.names(t); got != "arrived@0.9 arrived@1.0 tresor@1.0 tresor@1.1" {
			t.Fatalf("a new version: %q", got)
		}
		if err := p.rel.RemoveFollow(ctx, admin, "acme", "prod", "publisher:ci"); err != nil {
			t.Fatal(err)
		}
		if err := p.rel.RemoveFollow(ctx, admin, "acme", "prod", "publisher:ci"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("removed twice: %v", err)
		}
		// unfollowed: no new version, but the version held is completed (mode follow acts as manual)
		must(t, func() error {
			_, _, err := p.ten.AddVersion(ctx, admin, "v2.1.0", "release", []string{"v1.5.6", "v2.0.0"}, tenants.CoreExtensions{Names: []string{"json"}})
			return err
		})
		must(t, func() error {
			_, err := p.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.1.0"}, nil)
			return err
		})
		p.land(t, "tresor", "1.2")
		on21 := cpp("1.1")
		on21.DuckDBVersion = "v2.1.0"
		if _, _, err := p.pubs.AddToCatalogue(ctx, p.pub, "acme", bytes.NewReader(built(t, on21, "tresor_duckdb_cpp_init")),
			publish("tresor", "1.1", "linux_amd64")); err != nil {
			t.Fatal(err)
		}
		p.pass(t, true)
		if got := p.names(t); got != "arrived@0.9 arrived@1.0 tresor@1.0 tresor@1.1 tresor@1.1" {
			t.Fatalf("after the follow was removed: %q", got)
		}
		// a removed source's follows go with it
		if _, err := p.rel.SetFollow(ctx, admin, "acme", "staging", "publisher:other"); err != nil {
			t.Fatal(err)
		}
		must(t, func() error {
			return p.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeletePublisher(ctx, other.TenantID, "other", "os:1:t") })
		})
		if fs, err := p.rel.Follows(ctx, admin, "acme", "staging"); err != nil || len(fs) != 0 {
			t.Fatalf("a removed source's follow: %+v %v", fs, err)
		}
	})
}
