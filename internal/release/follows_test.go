package release_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

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
		out = append(out, rs[i].Name+"@"+rs[i].ExtVersion)
	}
	return strings.Join(out, " ")
}

// Spec 0033: a channel following a source takes every name of it the channel has no rule for, each
// new version as it lands; it skips a name the tenant uses from another source, one two follows
// offer, and a reserved one; once removed, nothing more is placed by it.
func TestFollow(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
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
		p.land(t, "tresor", "1.0")
		p.land(t, "geo", "1.0")
		p.land(t, "shared", "1.0")
		// the tenant serves geo from its own source in staging; the other publisher offers shared too
		if _, _, err := en.addTo(t, "staging", built(t, cpp("0.1"), "geo_duckdb_cpp_init"), release.AddOptions{Name: "geo"}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.rel.SetFollow(ctx, admin, "acme", "prod", "publisher:ci"); err != nil {
			t.Fatal(err)
		}
		if _, err := p.rel.SetFollow(ctx, admin, "acme", "prod", "publisher:other"); err != nil {
			t.Fatal(err)
		}
		otherPub := *p.pubs
		if _, _, err := otherPub.AddToCatalogue(ctx, authzOf(other), "acme", bytes.NewReader(built(t, cpp("2.0"), "shared_duckdb_cpp_init", "x")),
			publish("shared", "2.0", "linux_amd64")); err != nil {
			t.Fatal(err)
		}
		p.pass(t, false)
		if got := p.names(t); got != "tresor@1.0" {
			t.Fatalf("after the first pass: %q", got)
		}
		v, err := p.rel.Follow(ctx, admin, "acme", "prod", "publisher:ci")
		if err != nil || len(v.Skips) != 2 || !strings.Contains(v.Skips["geo"], "another source") || !strings.Contains(v.Skips["shared"], "contested") {
			t.Fatalf("the follow's skips: %+v %v", v, err)
		}
		s, err := p.rel.Source(ctx, admin, "acme", "prod", "tresor")
		if err != nil || s.Mode != store.RuleFollow {
			t.Fatalf("the follow's rule: %+v %v", s, err)
		}
		p.land(t, "tresor", "1.1")
		p.pass(t, false)
		if got := p.names(t); got != "tresor@1.0 tresor@1.1" {
			t.Fatalf("a new version: %q", got)
		}
		if err := p.rel.RemoveFollow(ctx, admin, "acme", "prod", "publisher:ci"); err != nil {
			t.Fatal(err)
		}
		if err := p.rel.RemoveFollow(ctx, admin, "acme", "prod", "publisher:ci"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("removed twice: %v", err)
		}
		p.land(t, "tresor", "1.2")
		p.pass(t, true)
		if got := p.names(t); got != "tresor@1.0 tresor@1.1" {
			t.Fatalf("after the follow was removed: %q", got)
		}
	})
}
