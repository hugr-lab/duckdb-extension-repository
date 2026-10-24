package release_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0031: a channel serves a name from one source. The first release sets it; another source's
// release is refused, naming it; a promotion keeps its source; a switch serves the other source's
// releases at once; the same body brought by the name's source takes the slot's release.
func TestSources(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.staging(t)
		pub := authz.Actor{Kind: authz.ActorPublisher, ID: "acme/ci", Tenant: "acme", Principals: auth.Principals{auth.PublisherKey("p-ci"): true}}
		pubs := *en.rel // a publisher's calls, allowed as its grants would
		pubs.Authz = allowAll{}
		v1 := built(t, cpp("1.0"), "tresor_duckdb_cpp_init")
		v2 := built(t, cpp("2.0"), "tresor_duckdb_cpp_init")
		served := func(channel string) []string {
			t.Helper()
			sc, err := en.st.GetServeChannel(ctx, "acme", channel)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := (&release.Snapshots{Store: en.st}).Get(ctx, sc.Channel)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, r := range snap.Releases {
				out = append(out, r.ExtVersion+"@"+r.Source)
			}
			return out
		}
		// a publisher's publication sets the name's source in staging
		r1, _, err := pubs.Add(ctx, pub, "acme", "staging", bytes.NewReader(v1), publish("tresor", "1.0", "linux_amd64"))
		if err != nil || r1.Source != "publisher:p-ci" {
			t.Fatalf("publish: %+v %v", r1, err)
		}
		// the tenant's release of the name is refused, naming the source
		var se *release.SourceError
		_, _, err = en.addTo(t, "staging", v2, release.AddOptions{Name: "tresor"})
		if !errors.Is(err, release.ErrOtherSource) || !errors.As(err, &se) || !strings.HasPrefix(se.Source, "publisher:") {
			t.Fatalf("another source: %v", err)
		}
		// a promotion keeps the source, and sets it in the target
		rs, _, err := en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "1.0"})
		if err != nil || len(rs) != 1 || rs[0].Source != "publisher:p-ci" {
			t.Fatalf("promote: %+v %v", rs, err)
		}
		if sel, _ := en.st.ChannelSources(ctx, channel(t, en).ID); sel["tresor"].Source != "publisher:p-ci" {
			t.Fatalf("prod's source: %+v", sel)
		}
		// switched to the tenant: its release lands and is the one served; the publisher's stays
		// stored, unserved
		v, err := en.rel.SetSource(ctx, admin, "acme", "staging", "tresor", "tenant")
		if err != nil || v.Source != "tenant" || v.Available["publisher:(removed)"] != 1 {
			t.Fatalf("switch: %+v %v", v, err)
		}
		if got := served("staging"); len(got) != 0 {
			t.Fatalf("served after the switch: %v", got)
		}
		if _, _, err := en.addTo(t, "staging", v2, release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		if got := served("staging"); strings.Join(got, " ") != "2.0@tenant" {
			t.Fatalf("served: %v", got)
		}
		// the same body as the publisher's, brought by the tenant: a release of the tenant's own, in the
		// tenant's slot (spec 0033: a slot is a source's)
		if r, existed, err := en.addTo(t, "staging", v1, release.AddOptions{Name: "tresor"}); err != nil || existed || r.Source != "tenant" {
			t.Fatalf("the same body again: %+v %v %v", r, existed, err)
		}
		if got := served("staging"); strings.Join(got, " ") != "2.0@tenant 1.0@tenant" {
			t.Fatalf("served: %v", got)
		}
		// cleared: nothing serves the name until the next release sets it again
		if err := en.rel.ClearSource(ctx, admin, "acme", "staging", "tresor"); err != nil {
			t.Fatal(err)
		}
		if err := en.rel.ClearSource(ctx, admin, "acme", "staging", "tresor"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cleared twice: %v", err)
		}
		if got := served("staging"); len(got) != 0 {
			t.Fatalf("served without a source: %v", got)
		}
		v3 := built(t, cpp("3.0"), "tresor_duckdb_cpp_init")
		if _, _, err := pubs.Add(ctx, pub, "acme", "staging", bytes.NewReader(v3), publish("tresor", "3.0", "linux_amd64")); err != nil {
			t.Fatal(err)
		}
		// the publisher's releases are served again, its 1.0 among them
		if got := served("staging"); strings.Join(got, " ") != "1.0@publisher:p-ci 3.0@publisher:p-ci" {
			t.Fatalf("served after the arrival: %v", got)
		}
		// a source the channel may not take
		for _, bad := range []string{"upstream:nope", "publisher:nope", "everyone"} {
			if _, err := en.rel.SetSource(ctx, admin, "acme", "staging", "tresor", bad); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", bad, err)
			}
		}
		if _, err := en.rel.SetSource(ctx, admin, "acme", "staging", "Bad-Name", "tenant"); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("a bad name: %v", err)
		}
		vs, err := en.rel.Sources(ctx, admin, "acme", "staging")
		if err != nil || len(vs) != 1 || vs[0].Name != "tresor" || vs[0].Source != "publisher:(removed)" || vs[0].Available["publisher:(removed)"] != 2 ||
			vs[0].Available["tenant"] != 2 {
			t.Fatalf("sources: %+v %v", vs, err)
		}
	})
}

// allowAll allows every call: a publisher's, as its grants would.
type allowAll struct{}

func (allowAll) Allow(context.Context, authz.Actor, authz.Verb, authz.Resource) error { return nil }

// Spec 0031: pull-through never offers a name another source holds in the channel.
func TestPullThroughSource(t *testing.T) {
	s := release.NewSnapshot(store.Channel{Kind: store.ChannelSigned}, nil, map[string][]store.CAPI{"v2.0.0": nil}, nil)
	s.PullThrough = []store.PullThrough{{UpstreamID: "u1", Platforms: map[string]bool{"linux_amd64": true}, Names: map[string]bool{"x": true}}}
	for sel, want := range map[string]string{"": "u1", "upstream:u1": "u1", "tenant": "", "upstream:u2": "", "publisher:p": ""} {
		s.Selected = map[string]string{}
		if sel != "" {
			s.Selected["x"] = sel
		}
		got := ""
		if p := s.PullThroughFor("v2.0.0", "linux_amd64", "x"); p != nil {
			got = p.UpstreamID
		}
		if got != want {
			t.Errorf("selected %q: %q, want %q", sel, got, want)
		}
	}
}

// Spec 0031: a cleared source is set again by the same body arriving again; a promotion takes only
// the releases the source channel serves, and is refused where another source holds the name; a
// slot's release taken by the name's source is recorded.
func TestSourcesPromotionAndClear(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.staging(t)
		snaps := &release.Snapshots{Store: en.st} // one cache: a change must bump release_version
		served := func(channel string) string {
			t.Helper()
			sc, err := en.st.GetServeChannel(ctx, "acme", channel)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := snaps.Get(ctx, sc.Channel)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, r := range snap.Releases {
				out = append(out, r.ExtVersion+"@"+r.Source)
			}
			return strings.Join(out, " ")
		}
		ch, err := en.st.GetChannel(ctx, "acme", "staging")
		if err != nil {
			t.Fatal(err)
		}
		pub := store.Publisher{TenantID: ch.TenantID, Name: "ci"}
		if err := en.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &pub) }); err != nil {
			t.Fatal(err)
		}
		pa := authz.Actor{Kind: authz.ActorPublisher, ID: "acme/ci", Tenant: "acme", Principals: auth.Principals{auth.PublisherKey(pub.ID): true}}
		pubs := *en.rel
		pubs.Authz = allowAll{}
		v1 := built(t, cpp("1.0"), "tresor_duckdb_cpp_init")
		v2 := built(t, cpp("2.0"), "tresor_duckdb_cpp_init")
		if _, _, err := en.addTo(t, "staging", v1, release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		if got := served("staging"); got != "1.0@tenant" {
			t.Fatalf("served: %q", got)
		}
		// cleared, then the same body again: the source is set again, and served
		if err := en.rel.ClearSource(ctx, admin, "acme", "staging", "tresor"); err != nil {
			t.Fatal(err)
		}
		if got := served("staging"); got != "" {
			t.Fatalf("served after the clear: %q", got)
		}
		if _, existed, err := en.addTo(t, "staging", v1, release.AddOptions{Name: "tresor"}); err != nil || !existed {
			t.Fatalf("the same body again: %v %v", existed, err)
		}
		if got := served("staging"); got != "1.0@tenant" {
			t.Fatalf("served after the same body again: %q", got)
		}
		// switched to the publisher, which publishes 2.0: 1.0 (the tenant's) is not promoted
		if _, err := en.rel.SetSource(ctx, admin, "acme", "staging", "tresor", "publisher:ci"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := pubs.Add(ctx, pa, "acme", "staging", bytes.NewReader(v2), publish("tresor", "2.0", "linux_amd64")); err != nil {
			t.Fatal(err)
		}
		if got := served("staging"); got != "2.0@publisher:"+pub.ID {
			t.Fatalf("served: %q", got)
		}
		if _, _, err := en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "1.0"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a release the source channel does not serve, promoted: %v", err)
		}
		// prod's name is the tenant's: the publisher's 2.0 is refused there, naming the source
		if _, err := en.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "tenant"); err != nil {
			t.Fatal(err)
		}
		_, _, err = en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "2.0"})
		var se *release.SourceError
		if !errors.As(err, &se) || se.Source != "tenant" {
			t.Fatalf("a promotion under another source: %v", err)
		}
		// the publisher brings the tenant's 1.0 body: a release of its own in its own slot (spec 0033),
		// the tenant's left as it was
		if r, existed, err := pubs.Add(ctx, pa, "acme", "staging", bytes.NewReader(v1), publish("tresor", "1.0", "linux_amd64")); err != nil || existed ||
			r.Source != "publisher:"+pub.ID {
			t.Fatalf("the publisher's same body: %+v %v %v", r, existed, err)
		}
		if got := served("staging"); got != "2.0@publisher:"+pub.ID+" 1.0@publisher:"+pub.ID {
			t.Fatalf("served: %q", got)
		}
		vs, err := en.rel.Sources(ctx, admin, "acme", "staging")
		if err != nil || len(vs) != 1 || vs[0].Available["tenant"] != 1 || vs[0].Available["publisher:ci"] != 2 {
			t.Fatalf("sources: %+v %v", vs, err)
		}
	})
}

// Spec 0031: two sources' first releases of a name at once: one sets the source, the other is
// refused. Both start together; only PostgreSQL and SQL Server really overlap the transactions
// (SQLite serialises its writers).
func TestSourceArrivalRace(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		pub := authz.Actor{Kind: authz.ActorPublisher, ID: "acme/ci", Tenant: "acme", Principals: auth.Principals{auth.PublisherKey("p-ci"): true}}
		pubs := *en.rel
		pubs.Authz = allowAll{}
		a := built(t, cpp("1.0"), "tresor_duckdb_cpp_init")
		b := built(t, cpp("2.0"), "tresor_duckdb_cpp_init")
		errs, start := make(chan error, 2), make(chan struct{})
		go func() { <-start; _, _, err := en.add(t, a, release.AddOptions{Name: "tresor"}); errs <- err }()
		go func() {
			<-start
			_, _, err := pubs.Add(ctx, pub, "acme", "prod", bytes.NewReader(b), publish("tresor", "2.0", "linux_amd64"))
			errs <- err
		}()
		close(start)
		e1, e2 := <-errs, <-errs
		ok, refused := 0, 0
		for _, err := range []error{e1, e2} {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, release.ErrOtherSource):
				refused++
			default:
				t.Fatal(err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("%v %v", e1, e2)
		}
	})
}
