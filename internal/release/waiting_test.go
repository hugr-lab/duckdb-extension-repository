package release_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// toCat publishes a version of a name into the catalogue only, as ci.
func (p placing) toCat(t *testing.T, name string, m extfile.Metadata) store.Build {
	t.Helper()
	b, _, err := p.pubs.AddToCatalogue(ctx, p.pub, "acme", bytes.NewReader(built(t, m, name+"_duckdb_cpp_init")),
		publish(name, m.ExtensionVersion, m.Platform))
	if err != nil {
		t.Fatalf("publish %s %s into the catalogue: %v", name, m.ExtensionVersion, err)
	}
	return b
}

// view is prod's rule for a name.
func (p placing) view(t *testing.T, name string) release.SourceView {
	t.Helper()
	v, err := p.rel.Source(ctx, admin, "acme", "prod", name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// waitingEvents are the tenant's source.waiting events, oldest first, as name version current mode.
func (p placing) waitingEvents(t *testing.T) []string {
	t.Helper()
	evs, err := p.st.ListEvents(ctx, p.ci.TenantID, store.EventFilter{Kind: "source.waiting", Ascending: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		var d struct{ Source, Version, Current, Mode string }
		if err := json.Unmarshal([]byte(e.Data), &d); err != nil {
			t.Fatal(err)
		}
		if e.Actor != "system:placement" || d.Source != "publisher:ci" {
			t.Fatalf("an event: %+v", e)
		}
		out = append(out, strings.TrimPrefix(e.Subject, "channel:prod/ext:")+" "+d.Version+" "+d.Current+" "+d.Mode)
	}
	return out
}

// Spec 0041: what waits under a rule is what the pass could take: not a version for a DuckDB
// version the channel does not serve, nor one the channel had (yanked, purged), nor one held by its
// source; under an auto rule, waits_until is the later of the source's hold and the channel's, and
// what waited is what the pass places when it ends.
func TestWaiting(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clock := pastClock()
		en.st.Now = func() time.Time { return clock }
		p := newPlacing(t, en)
		p.toCat(t, "tresor", cpp("1.0"))
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "manual"); err != nil {
			t.Fatal(err)
		}
		if v := p.view(t, "tresor"); v.Waiting != "1.0" || !v.WaitsUntil.IsZero() {
			t.Fatalf("a manual rule's waiting version: %+v", v)
		}
		if _, _, err := p.rel.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: "1.0"}); err != nil {
			t.Fatal(err)
		}
		if v := p.view(t, "tresor"); v.Waiting != "" {
			t.Fatalf("after placing it: %+v", v)
		}
		// a version for a DuckDB version prod does not serve
		must(t, func() error {
			_, _, err := p.ten.AddVersion(ctx, admin, "v2.1.0", "release", []string{"v1.5.6", "v2.0.0"}, tenants.CoreExtensions{Names: []string{"json"}})
			return err
		})
		m := cpp("1.1")
		m.DuckDBVersion = "v2.1.0"
		p.toCat(t, "tresor", m)
		if v := p.view(t, "tresor"); v.Waiting != "" {
			t.Fatalf("a version for a DuckDB version the channel does not serve: %+v", v)
		}
		// one the channel had: yanked, then purged
		p.toCat(t, "tresor", cpp("1.2"))
		if v := p.view(t, "tresor"); v.Waiting != "1.2" {
			t.Fatalf("1.2: %+v", v)
		}
		rels, _, err := p.rel.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: "1.2"})
		if err != nil {
			t.Fatal(err)
		}
		y, err := p.rel.Apply(ctx, admin, "acme", "prod", "tresor", rels[0].ID, release.Yank, 0)
		if err != nil {
			t.Fatal(err)
		}
		if v := p.view(t, "tresor"); v.Waiting != "" || v.Current != "1.0" {
			t.Fatalf("a yanked version: %+v", v)
		}
		if _, err := p.rel.Purge(ctx, admin, "acme", "prod", "tresor", y.ID, 0); err != nil {
			t.Fatal(err)
		}
		if v := p.view(t, "tresor"); v.Waiting != "" {
			t.Fatalf("a purged version: %+v", v)
		}
		// an auto rule, its source's hold an hour, the channel's three: held, then waiting, then placed
		p.hold(t, time.Hour)
		if _, err := p.rel.SetChannelHold(ctx, admin, "acme", "prod", 3*time.Hour); err != nil {
			t.Fatal(err)
		}
		b := p.toCat(t, "tresor", cpp("1.3"))
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "auto"); err != nil {
			t.Fatal(err)
		}
		if v := p.view(t, "tresor"); v.Waiting != "" || !v.WaitsUntil.Equal(b.CreatedAt.Add(3*time.Hour)) {
			t.Fatalf("held by its source: %+v", v)
		}
		clock = clock.Add(61 * time.Minute)
		if v := p.view(t, "tresor"); v.Waiting != "1.3" || !v.WaitsUntil.Equal(b.CreatedAt.Add(3*time.Hour)) {
			t.Fatalf("within the channel's hold: %+v", v)
		}
		p.pass(t, true)
		if got := p.prod(t); strings.Contains(got, "1.3") {
			t.Fatalf("placed within the channel's hold: %q", got)
		}
		clock = clock.Add(2 * time.Hour)
		p.pass(t, false)
		if v := p.view(t, "tresor"); !strings.Contains(p.prod(t), "1.3@v2.0.0:active") || v.Waiting != "" || !v.WaitsUntil.IsZero() {
			t.Fatalf("what waited, placed: %q %+v", p.prod(t), v)
		}
		// a blocked body does not wait
		p.toCat(t, "tresor", cpp("1.4"))
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "manual"); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(4 * time.Hour)
		vs, err := p.rel.CatalogueVersions(ctx, admin, "acme", "duckdb_extension", "publisher:ci", "tresor")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.rel.Block(ctx, admin, "acme", vs[0].Builds[0].BodyHash, "CVE"); err != nil {
			t.Fatal(err)
		}
		if v := p.view(t, "tresor"); v.Waiting != "" {
			t.Fatalf("a blocked body: %+v", v)
		}
	})
}

// Spec 0041: the pass tells of a version waiting under a manual rule once per version (a newer one
// again; one arriving held when its hold ends, woken by due_at; after a mode change and back again),
// never under an auto rule nor a follow's while it follows; a follow's rule after the follow is
// removed tells; a failed tell is logged, never the rule's outcome; two looks reading one
// told_version tell once.
func TestSourceWaiting(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clock := pastClock()
		en.st.Now = func() time.Time { return clock }
		en.rel.Authz = publishes{"tresor": true, "geo": true, "tiles": true} // a follow takes the names ci may publish
		p := newPlacing(t, en)
		want := func(what string, events ...string) {
			t.Helper()
			if got := strings.Join(p.waitingEvents(t), ", "); got != strings.Join(events, ", ") {
				t.Fatalf("%s: %q", what, got)
			}
		}
		p.toCat(t, "tresor", cpp("1.0"))
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "manual"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, false)
		want("a manual rule", "tresor 1.0  manual")
		p.pass(t, true)
		want("a second look", "tresor 1.0  manual")
		if _, _, err := p.rel.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: "1.0"}); err != nil {
			t.Fatal(err)
		}
		p.toCat(t, "tresor", cpp("1.1"))
		p.pass(t, false)
		want("a newer version", "tresor 1.0  manual", "tresor 1.1 1.0 manual")
		// a version arriving held: told when its hold ends, the pass woken by due_at
		p.hold(t, time.Hour)
		p.toCat(t, "tresor", cpp("1.2"))
		p.pass(t, false)
		want("held", "tresor 1.0  manual", "tresor 1.1 1.0 manual")
		clock = clock.Add(61 * time.Minute)
		p.pass(t, false)
		want("its hold ended", "tresor 1.0  manual", "tresor 1.1 1.0 manual", "tresor 1.2 1.0 manual")
		p.hold(t, 0)
		// a mode change and back tells again
		for _, mode := range []string{"auto", "manual"} {
			if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", mode); err != nil {
				t.Fatal(err)
			}
		}
		p.pass(t, false)
		want("a mode change and back", "tresor 1.0  manual", "tresor 1.1 1.0 manual", "tresor 1.2 1.0 manual", "tresor 1.2 1.0 manual")
		told := p.waitingEvents(t)

		// an auto rule whose version waits for the channel's hold: no tell
		if _, err := p.rel.SetChannelHold(ctx, admin, "acme", "prod", 3*time.Hour); err != nil {
			t.Fatal(err)
		}
		p.toCat(t, "geo", cpp("1.0"))
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "geo", "publisher:ci", "auto"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, true)
		if v := p.view(t, "geo"); v.Waiting != "1.0" {
			t.Fatalf("geo waits: %+v", v)
		}
		want("an auto rule", told...)
		if _, err := p.rel.SetChannelHold(ctx, admin, "acme", "prod", 0); err != nil {
			t.Fatal(err)
		}

		// a follow's rule: auto while the channel follows (no tell), manual once it does not
		p.toCat(t, "tiles", cpp("1.0"))
		if _, err := p.rel.SetFollow(ctx, admin, "acme", "prod", "publisher:ci"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, false)
		if v := p.view(t, "tiles"); v.Mode != store.RuleFollow || v.Current != "1.0" {
			t.Fatalf("the follow took tiles: %+v", v)
		}
		p.toCat(t, "tiles", cpp("1.1"))
		clock = clock.Add(time.Minute)
		if err := p.rel.RemoveFollow(ctx, admin, "acme", "prod", "publisher:ci"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, true)
		want("a follow's rule after the follow is removed", append(told, "tiles 1.1 1.0 follow")...)
		told = p.waitingEvents(t)

		// a failed tell is logged, not the rule's outcome; the next look tells
		release.SetBeforeTell(p.rel, func() error { return errors.New("the store is gone") })
		p.toCat(t, "tresor", cpp("1.5"))
		var log bytes.Buffer
		(&release.Placer{Service: p.rel, Holder: "test", Log: slog.New(slog.NewTextHandler(&log, nil))}).Once(ctx, false)
		if v := p.view(t, "tresor"); v.Outcome != "" || !strings.Contains(log.String(), "telling of a version waiting") {
			t.Fatalf("a failed tell: %+v %s", v, log.String())
		}
		want("a failed tell", told...)
		release.SetBeforeTell(p.rel, nil)
		p.pass(t, true)
		want("the next look", append(told, "tresor 1.5 1.0 manual")...)
		told = p.waitingEvents(t)

		// two looks reading one told_version (a lapsed lease) tell once
		p.toCat(t, "tresor", cpp("1.6"))
		rules, err := p.st.DueRules(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			if r.Name != "tresor" {
				continue
			}
			clock = clock.Add(time.Second)
			for range 2 {
				if err := release.LookAndTell(ctx, p.rel, r); err != nil {
					t.Fatal(err)
				}
			}
		}
		want("two looks racing", append(told, "tresor 1.6 1.0 manual")...)
	})
}

// Spec 0041: the tenant's own source is never the pass's: what waits under its rule is in the
// sources answer, and no event tells of it.
func TestWaitingTenant(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		if _, _, err := en.rel.AddToCatalogue(ctx, admin, "acme", bytes.NewReader(built(t, cpp("1.0"), "acl_duckdb_cpp_init")),
			release.AddOptions{Name: "acl"}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "acl", "tenant", "manual"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, true)
		if v := p.view(t, "acl"); v.Waiting != "1.0" {
			t.Fatalf("the tenant's waiting version: %+v", v)
		}
		if evs := p.waitingEvents(t); len(evs) != 0 {
			t.Fatalf("a tell for the tenant's source: %v", evs)
		}
	})
}
