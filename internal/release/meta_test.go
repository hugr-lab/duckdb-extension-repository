package release_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// metaRoles allows a tenant's administrator (acme/admin) admin, a distribution admin distribute,
// publishers publish, and the OS everything.
type metaRoles struct{}

func (metaRoles) Allow(ctx context.Context, a authz.Actor, v authz.Verb, r authz.Resource) error {
	switch {
	case a.Kind == authz.ActorPrincipal && a.ID == "acme/admin" && v == authz.VerbAdmin && r.Tenant == "acme",
		a.Kind == authz.ActorDistributionAdmin && v == authz.VerbDistribute,
		a.Kind == authz.ActorPublisher && v == authz.VerbPublish:
		return nil
	}
	return authz.ServerAdmin{}.Allow(ctx, a, v, r)
}

var (
	tenantAdmin = authz.Actor{Kind: authz.ActorPrincipal, ID: "acme/admin", Tenant: "acme", Principals: auth.Principals{}}
	distAdmin   = authz.Actor{Kind: authz.ActorDistributionAdmin, ID: "kc|dist"}
)

func pngOf(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = uint8(i)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func str(s string) *string { return &s }

func newIcon(t *testing.T, side int) *release.IconInput {
	return &release.IconInput{ContentType: "image/png", Data: pngOf(t, side)}
}

// metaEnv is a tenant with entries from three sources: the tenant's own (acl, an administrator's
// add), its own publisher ci (tile), and a distributor's publisher enterest-ci (geo).
func metaEnv(t *testing.T, en *env) (placing, store.Publisher, authz.Actor) {
	t.Helper()
	p, land := distributorPlacing(t, en)
	en.rel.Authz = metaRoles{}
	must(t, func() error { return land("geo", "1.0", "linux_amd64") })
	if _, _, err := en.rel.AddToCatalogue(ctx, admin, "acme", bytes.NewReader(built(t, cpp("1.0"), "acl_duckdb_cpp_init")),
		release.AddOptions{Name: "acl"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.pubs.AddToCatalogue(ctx, p.pub, "acme", bytes.NewReader(built(t, cpp("1.0"), "tile_duckdb_cpp_init")),
		publish("tile", "1.0", "linux_amd64")); err != nil {
		t.Fatal(err)
	}
	var dist store.Publisher
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			dist, err = tx.GetPublisher(ctx, p.ci.TenantID, "enterest-ci")
			return err
		})
	})
	return p, dist, authzOf(dist)
}

func TestCheckSummary(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		"   ":                             "",
		" Geometry tools ":                "Geometry tools",
		"Cafe\u0301":                      "Caf\u00E9", // NFC
		"\u00DCn\u00EFcode \U0001F30D ok": "\u00DCn\u00EFcode \U0001F30D ok",
		"q\u0301\u0302\u0303\u0304":       "q\u0301\u0302\u0303\u0304", // four marks (q has no precomposed form)
	} {
		if got, err := release.CheckSummary(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for name, in := range map[string]string{
		"161 characters":   strings.Repeat("x", 161),
		"a newline":        "two\nlines",
		"a tab":            "a\ttab",
		"U+2028":           "line\u2028separator",
		"U+2029":           "para\u2029separator",
		"RLO (bidi)":       "evil\u202Etxt.exe",
		"LRM":              "a\u200Eb",
		"zero width space": "a\u200Bb",
		"a tag character":  "a\U000E0041b",
		"U+115F":           "a\u115Fb",
		"U+1160":           "a\u1160b",
		"U+3164":           "a\u3164b",
		"U+FFA0":           "a\uFFA0b",
		"U+2800":           "a\u2800b",
		"no-break space":   "a\u00A0b",
		"five marks":       "q\u0301\u0302\u0303\u0304\u0305",
		"invalid UTF-8":    "a\xffb",
	} {
		if _, err := release.CheckSummary(in); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if got, err := release.CheckSummary(strings.Repeat("\U0001F30D", 160)); err != nil || len([]rune(got)) != 160 {
		t.Errorf("160 code points: %v", err)
	}
}

// Spec 0036: a write replaces the whole metadata; the same write again changes nothing and makes no
// event; null and "" clear; an icon is kept by its hash, and a hash that changed is a conflict; a
// new icon is checked; the icon is read back checked; the catalogue answers the metadata; a
// re-publication after a purge finds it again.
func TestEntryMetaWrites(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p, _, _ := metaEnv(t, en)
		rel := en.rel
		events := func() int {
			tn, _ := en.st.GetTenant(ctx, "acme")
			evs, err := en.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: "catalogue.meta", Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			return len(evs)
		}
		ic := newIcon(t, 128)
		v, err := rel.SetEntryMeta(ctx, tenantAdmin, "acme", "", "tenant", "acl", release.MetaWrite{Summary: str(" Access control "), Icon: ic})
		if err != nil || v.Summary != "Access control" || v.IconWidth != 128 || v.IconBytes != int64(len(ic.Data)) || v.SetBy != tenantAdmin.String() {
			t.Fatalf("the first write: %+v %v", v, err)
		}
		if events() != 1 {
			t.Fatalf("events: %d", events())
		}
		got, err := rel.EntryIcon(ctx, tenantAdmin, "acme", "", "tenant", "acl")
		if err != nil || !bytes.Equal(got, ic.Data) {
			t.Fatalf("the icon read back: %v", err)
		}
		// the same again: nothing changes, no event (a token's write still moves the mark)
		again, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl",
			release.MetaWrite{Summary: str("Access control"), Icon: &release.IconInput{Keep: v.IconSHA256}})
		if err != nil || again.SetBy != v.SetBy || !again.SetAt.Equal(v.SetAt) || events() != 1 {
			t.Fatalf("the same write: %+v %v (%d events)", again, err, events())
		}
		// the summary alone, the icon kept by its hash
		v2, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl",
			release.MetaWrite{Summary: str("ACLs"), Icon: &release.IconInput{Keep: v.IconSHA256}})
		if err != nil || v2.IconSHA256 != v.IconSHA256 || v2.Summary != "ACLs" || events() != 2 {
			t.Fatalf("the summary alone: %+v %v", v2, err)
		}
		if _, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl",
			release.MetaWrite{Icon: &release.IconInput{Keep: strings.Repeat("0", 64)}}); !errors.Is(err, release.ErrIconChanged) {
			t.Fatalf("a kept icon that changed: %v", err)
		}
		// the catalogue answers it
		es, err := rel.Catalogue(ctx, admin, "acme", pkgtype.Default, "tenant", "acl")
		if err != nil || len(es) != 1 || es[0].Meta.Summary != "ACLs" || es[0].Meta.IconSHA256 != v.IconSHA256 || es[0].Distributor != "" {
			t.Fatalf("the catalogue: %+v %v", es, err)
		}
		es, _ = rel.Catalogue(ctx, admin, "acme", pkgtype.Default, "publisher:enterest-ci", "geo")
		if len(es) != 1 || es[0].Distributor != "enterest" {
			t.Fatalf("a distributor's entry: %+v", es)
		}
		// cleared: null and ""
		v3, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{Summary: str("  ")})
		if err != nil || v3.Summary != "" || v3.IconSHA256 != "" || events() != 3 {
			t.Fatalf("cleared: %+v %v", v3, err)
		}
		if _, err := rel.EntryIcon(ctx, admin, "acme", "", "tenant", "acl"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("no icon: %v", err)
		}
		// refused icons
		for name, in := range map[string]*release.IconInput{
			"a JPEG type":  {ContentType: "image/jpeg", Data: ic.Data},
			"too small":    newIcon(t, 32),
			"not a PNG":    {ContentType: "image/png", Data: []byte("<svg/>")},
			"new and kept": {ContentType: "image/png", Data: ic.Data, Keep: v.IconSHA256},
		} {
			if _, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{Icon: in}); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// the row outlives the builds: a re-publication after a purge finds it
		_, err = rel.SetEntryMeta(ctx, p.pub, "acme", "", "publisher:ci", "tile", release.MetaWrite{Summary: str("Tiles"),
			SignedAt: time.Now().UTC().Truncate(time.Second), BodySHA256: strings.Repeat("a", 64)})
		if err != nil {
			t.Fatal(err)
		}
		bs, err := en.st.SourceBuilds(ctx, p.ci.TenantID, "duckdb_extension", store.PublisherSource(p.ci.ID), "tile")
		if err != nil || len(bs) != 1 {
			t.Fatalf("tile's builds: %v %v", bs, err)
		}
		if _, err := rel.PurgeEntry(ctx, admin, "acme", bs[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := rel.EntryMeta(ctx, admin, "acme", "", "publisher:ci", "tile"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("an entry without builds: %v", err)
		}
		if _, _, err := p.pubs.AddToCatalogue(ctx, p.pub, "acme", bytes.NewReader(built(t, cpp("1.1"), "tile_duckdb_cpp_init")),
			publish("tile", "1.1", "linux_amd64")); err != nil {
			t.Fatal(err)
		}
		if m, err := rel.EntryMeta(ctx, admin, "acme", "", "publisher:ci", "tile"); err != nil || m.Summary != "Tiles" {
			t.Fatalf("after a re-publication: %+v %v", m, err)
		}
	})
}

// Spec 0036: who reaches an entry's metadata. A tenant's administrators the tenant's own sources,
// and a distributor's entries to read only; a distribution admin a distributor's entries only; a
// publisher its own source only, by either name, and a distributor's publisher a name it may
// publish; an entry must exist. Every refusal is a not-found.
func TestEntryMetaAccess(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p, dist, distPub := metaEnv(t, en)
		rel := en.rel
		w := release.MetaWrite{Summary: str("x")}
		refused := func(name string, err error) {
			t.Helper()
			if !errors.Is(err, authz.ErrDenied) && !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		allowed := func(name string, err error) {
			t.Helper()
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		write := func(a authz.Actor, source, name string) error {
			_, err := rel.SetEntryMeta(ctx, a, "acme", "", source, name, w)
			return err
		}
		read := func(a authz.Actor, source, name string) error {
			_, err := rel.EntryMeta(ctx, a, "acme", "", source, name)
			return err
		}
		allowed("a tenant administrator, the tenant's", write(tenantAdmin, "tenant", "acl"))
		allowed("a tenant administrator, its own publisher's", write(tenantAdmin, "publisher:ci", "tile"))
		refused("a tenant administrator, a distributor's", write(tenantAdmin, "publisher:enterest-ci", "geo"))
		allowed("a tenant administrator reads a distributor's", read(tenantAdmin, "publisher:enterest-ci", "geo"))
		refused("another principal", write(authz.Actor{Kind: authz.ActorPrincipal, ID: "acme/dev", Tenant: "acme"}, "tenant", "acl"))
		allowed("a distribution admin, a distributor's", write(distAdmin, "publisher:enterest-ci", "geo"))
		allowed("a distribution admin, by id", write(distAdmin, "publisher-id:"+dist.ID, "geo"))
		refused("a distribution admin, the tenant's", write(distAdmin, "tenant", "acl"))
		refused("a distribution admin reads the tenant's", read(distAdmin, "publisher:ci", "tile"))
		allowed("a server administrator, a distributor's", write(admin, "publisher:enterest-ci", "geo"))
		allowed("a server administrator, the tenant's", write(admin, "publisher:ci", "tile"))
		allowed("a distributor's publisher, its own", write(distPub, "publisher:enterest-ci", "geo"))
		allowed("a distributor's publisher, by id", write(distPub, "publisher-id:"+dist.ID, "geo"))
		refused("a distributor's publisher, another source", write(distPub, "publisher:ci", "tile"))
		refused("a distributor's publisher, the tenant's", write(distPub, "tenant", "acl"))
		allowed("the tenant's publisher, its own", write(p.pub, "publisher:ci", "tile"))
		refused("the tenant's publisher, a distributor's", write(p.pub, "publisher:enterest-ci", "geo"))
		refused("another tenant's publisher", func() error {
			a := p.pub
			a.Tenant = "other"
			_, err := rel.SetEntryMeta(ctx, a, "acme", "", "publisher:ci", "tile", w)
			return err
		}())
		refused("no entry", write(admin, "tenant", "nope"))
		refused("no entry of the type", func() error {
			defer pkgtype.RegisterForTest("test_package")()
			_, err := rel.SetEntryMeta(ctx, admin, "acme", "test_package", "tenant", "acl", w)
			return err
		}())
		refused("no source", write(admin, "upstream:nope", "acl"))
		refused("a licence manager", write(authz.Actor{Kind: authz.ActorLicenseManager, ID: "kc|lm"}, "tenant", "acl"))
		if es, err := rel.Catalogue(ctx, distPub, "acme", pkgtype.Default, "", ""); err != nil || len(es) != 1 || es[0].Meta.Summary != "x" {
			t.Errorf("a distributor's publisher's list with its metadata: %+v %v", es, err)
		}
		// a name reserved server-wide since: the distributor's publisher no longer reaches it; the
		// distribution admin still does
		t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
		reserveServerWide(t, en.st, "geo")
		refused("a distributor's publisher, a reserved name", write(distPub, "publisher:enterest-ci", "geo"))
		refused("a distributor's publisher reads a reserved name", read(distPub, "publisher:enterest-ci", "geo"))
		if es, err := rel.Catalogue(ctx, distPub, "acme", pkgtype.Default, "", ""); err != nil || len(es) != 1 || es[0].Meta.Summary != "" {
			t.Errorf("a distributor's publisher's list, a reserved name's metadata: %+v %v", es, err)
		}
		if es, _ := rel.Catalogue(ctx, admin, "acme", pkgtype.Default, "publisher:enterest-ci", "geo"); len(es) != 1 || es[0].Meta.Summary != "x" {
			t.Errorf("a server administrator's list: %+v", es)
		}
		allowed("a distribution admin, a reserved name", write(distAdmin, "publisher:enterest-ci", "geo"))
	})
}

// Spec 0036: replay. A signed write older than the entry's last, or of its second with another
// body, is stale; the same request again changes nothing; every accepted signed write advances the
// mark, and a token's write moves it to the server's clock. A revoked key's write writes nothing.
func TestEntryMetaStale(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		_, _, distPub := metaEnv(t, en)
		rel := en.rel
		t0 := time.Now().UTC().Truncate(time.Second)
		body := func(c string) string { return strings.Repeat(c, 64) }
		signed := func(at time.Time, b, summary string) error {
			_, err := rel.SetEntryMeta(ctx, distPub, "acme", "", "publisher:enterest-ci", "geo",
				release.MetaWrite{Summary: str(summary), SignedAt: at, BodySHA256: b, KeyFingerprint: "SHA256:k"})
			return err
		}
		must(t, func() error { return signed(t0, body("a"), "one") })
		if err := signed(t0.Add(-time.Second), body("b"), "older"); !errors.Is(err, release.ErrStale) {
			t.Fatalf("an older request: %v", err)
		}
		if err := signed(t0, body("b"), "same second"); !errors.Is(err, release.ErrStale) {
			t.Fatalf("another body of the same second: %v", err)
		}
		must(t, func() error { return signed(t0, body("a"), "one") })                  // the same request again
		must(t, func() error { return signed(t0.Add(time.Second), body("c"), "one") }) // a no-op advances the mark
		if err := signed(t0, body("a"), "one"); !errors.Is(err, release.ErrStale) {
			t.Fatalf("the first request after a no-op: %v", err)
		}
		m, err := rel.EntryMeta(ctx, admin, "acme", "", "publisher:enterest-ci", "geo")
		if err != nil || !m.SignedAt.Equal(t0.Add(time.Second)) || m.SignedBody != body("c") || m.Summary != "one" {
			t.Fatalf("the mark: %+v %v", m, err)
		}
		// a token's write: the mark is the server's clock, so no request signed before it undoes it
		en.st.Now = func() time.Time { return t0.Add(time.Minute) }
		if _, err := rel.SetEntryMeta(ctx, distAdmin, "acme", "", "publisher:enterest-ci", "geo", release.MetaWrite{Summary: str("two")}); err != nil {
			t.Fatal(err)
		}
		if err := signed(t0.Add(30*time.Second), body("d"), "one"); !errors.Is(err, release.ErrStale) {
			t.Fatalf("a request signed before a token's write: %v", err)
		}
		m, _ = rel.EntryMeta(ctx, admin, "acme", "", "publisher:enterest-ci", "geo")
		if !m.SignedAt.Equal(t0.Add(time.Minute)) || m.SignedBody != "" || m.Summary != "two" || m.SetBy != distAdmin.String() {
			t.Fatalf("after a token's write: %+v", m)
		}
		// the event names the signing key
		tn, _ := en.st.GetTenant(ctx, "acme")
		evs, _ := en.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: "catalogue.meta", Limit: 10})
		if len(evs) != 2 || !strings.Contains(evs[len(evs)-1].Data+evs[0].Data, `"key":"SHA256:k"`) {
			t.Fatalf("the events: %+v", evs)
		}
		// a revoked key: nothing is written
		_, err = rel.SetEntryMeta(ctx, distPub, "acme", "", "publisher:enterest-ci", "geo",
			release.MetaWrite{Summary: str("three"), SignedAt: t0.Add(2 * time.Minute), BodySHA256: body("e"), KeyID: store.NewID()})
		if !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("a revoked key's write: %v", err)
		}
	})
}

// Spec 0036: an icon counts toward its source's byte cap; a publisher's or a distribution admin's
// write beyond it is refused, an administrator's is not bounded.
func TestEntryMetaCatalogueFull(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		_, _, distPub := metaEnv(t, en)
		rel := en.rel
		tn, _ := en.st.GetTenant(ctx, "acme")
		dist, _ := en.st.ResolveSource(ctx, tn.ID, "publisher:enterest-ci")
		_, used, err := en.st.SourceUsage(ctx, tn.ID, dist)
		if err != nil {
			t.Fatal(err)
		}
		ic := newIcon(t, 64)
		rel.MaxBytes = used + int64(len(ic.Data)) - 1
		for _, a := range []authz.Actor{distPub, distAdmin} {
			if _, err := rel.SetEntryMeta(ctx, a, "acme", "", "publisher:enterest-ci", "geo", release.MetaWrite{Icon: ic}); !errors.Is(err, store.ErrCatalogueFull) {
				t.Fatalf("%s beyond the cap: %v", a.Kind, err)
			}
		}
		v, err := rel.SetEntryMeta(ctx, admin, "acme", "", "publisher:enterest-ci", "geo", release.MetaWrite{Icon: ic})
		if err != nil {
			t.Fatalf("a server administrator is not bounded: %v", err)
		}
		if _, n, _ := en.st.SourceUsage(ctx, tn.ID, dist); n != used+int64(len(ic.Data)) {
			t.Fatalf("the usage counts the icon: %d", n)
		}
		// the same icon again, by its bytes: no growth, so within the cap
		rel.MaxBytes = used + int64(len(ic.Data))
		later := time.Now().UTC().Truncate(time.Second).Add(time.Minute) // past the token write's mark
		if _, err := rel.SetEntryMeta(ctx, distPub, "acme", "", "publisher:enterest-ci", "geo", release.MetaWrite{Icon: ic,
			SignedAt: later, BodySHA256: strings.Repeat("f", 64)}); err != nil {
			t.Fatalf("the same icon again: %v", err)
		}
		if _, err := rel.SetEntryMeta(ctx, distPub, "acme", "", "publisher:enterest-ci", "geo", release.MetaWrite{
			Icon: &release.IconInput{Keep: v.IconSHA256}, SignedAt: later.Add(time.Second),
			BodySHA256: strings.Repeat("e", 64)}); err != nil {
			t.Fatalf("kept by its hash: %v", err)
		}
	})
}
