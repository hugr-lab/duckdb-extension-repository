package release_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0038: a room check's failure to read the catalogue's use is ErrUsage (an upstream's cell
// failed, never rejected); its refusals stay as they are.
func TestUsageErr(t *testing.T) {
	read := errors.New("connection reset")
	if err := release.UsageErr(read); !errors.Is(err, release.ErrUsage) || !errors.Is(err, read) {
		t.Fatalf("a read error: %v", err)
	}
	for _, refusal := range []error{store.LimitRefused(store.LimitBytes, 1), store.ErrCatalogueFull} {
		if err := release.UsageErr(refusal); err != refusal {
			t.Fatalf("a refusal: %v", err)
		}
	}
	if release.UsageErr(nil) != nil {
		t.Fatal("nil")
	}
}

// Spec 0038: at a full tenant, a publication and an administrator's add (which the per-source cap
// exempts) are refused by its bytes limit; the same body again, a bundle import (WithoutLimits) and,
// once a release is purged (before the collector runs), a new body pass.
func TestTenantBytesLimit(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		p.publish(t, cpp("1.0"))
		tn, _ := p.st.GetTenant(ctx, "acme")
		used, err := p.st.TenantBytes(ctx, tn.ID)
		if err != nil || used == 0 {
			t.Fatalf("the tenant's bytes: %d %v", used, err)
		}
		if _, _, err := p.ten.SetTenantLimits(ctx, admin, "acme", map[string]int64{store.LimitBytes: used}, ""); err != nil {
			t.Fatal(err)
		}
		var le *store.LimitError
		if _, _, err := p.catalogueOnly(t, cpp("1.1")); !errors.As(err, &le) || le.Limit != store.LimitBytes {
			t.Fatalf("a publication at a full tenant: %v", err)
		}
		if _, _, err := p.pubs.Add(ctx, p.pub, "acme", "staging", bytes.NewReader(built(t, cpp("1.1"), "tresor_duckdb_cpp_init")),
			publish("tresor", "1.1", "linux_amd64")); !errors.Is(err, store.ErrTenantLimit) {
			t.Fatalf("a publication into a channel at a full tenant: %v", err)
		}
		p.pubs.MaxEntries = 1 // the per-source cap exempts an administrator's add; the tenant limit does not
		add := func(c []byte) error {
			_, _, err := p.rel.AddToCatalogue(ctx, admin, "acme", bytes.NewReader(c), release.AddOptions{Name: "tresor"})
			return err
		}
		if err := add(built(t, cpp("3.0"), "tresor_duckdb_cpp_init")); !errors.Is(err, store.ErrTenantLimit) {
			t.Fatalf("an administrator's add at a full tenant: %v", err)
		}
		p.pubs.MaxEntries = 0
		// the same body again is no growth
		if _, existed, err := p.catalogueOnly(t, cpp("1.0")); err != nil || !existed {
			t.Fatalf("the same body again: %v %v", existed, err)
		}
		// a bundle import is counted, never refused
		if _, _, err := p.rel.AddToCatalogue(store.WithoutLimits(ctx), admin, "acme", bytes.NewReader(built(t, cpp("4.0"), "tresor_duckdb_cpp_init")),
			release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatalf("a bundle import: %v", err)
		}
		if now, _ := p.st.TenantBytes(ctx, tn.ID); now <= used {
			t.Fatalf("the import counted: %d", now)
		}
		// room again: the import's entry purged, the published release yanked and purged
		bs, _ := p.st.SourceBuilds(ctx, tn.ID, "duckdb_extension", store.SourceTenant, "tresor")
		for _, b := range bs {
			if _, err := p.rel.PurgeEntry(ctx, admin, "acme", b.ID); err != nil {
				t.Fatal(err)
			}
		}
		ch, _ := p.st.GetChannel(ctx, "acme", "staging")
		rs, _ := p.st.ListReleases(ctx, ch.ID, "tresor")
		for _, r := range rs {
			if _, err := p.rel.Apply(ctx, admin, "acme", "staging", "tresor", r.ID, release.Yank, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := p.rel.Purge(ctx, admin, "acme", "staging", "tresor", r.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
		if now, _ := p.st.TenantBytes(ctx, tn.ID); now != 0 {
			t.Fatalf("after the purges, before the collector: %d", now)
		}
		if _, _, err := p.catalogueOnly(t, cpp("1.1")); err != nil {
			t.Fatalf("a publication with room again: %v", err)
		}
	})
}

// Spec 0038: an icon's growth counts against its tenant's bytes limit, for every writer of an own
// source's entry (an administrator too); a smaller icon passes; a distributor's icon is outside it.
func TestTenantBytesLimitIcons(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		metaEnv(t, en)
		rel := en.rel
		tn, _ := en.st.GetTenant(ctx, "acme")
		used, err := en.st.TenantBytes(ctx, tn.ID)
		if err != nil {
			t.Fatal(err)
		}
		big, small := newIcon(t, 128), newIcon(t, 64)
		if _, _, err := en.ten.SetTenantLimits(ctx, admin, "acme", map[string]int64{store.LimitBytes: used + int64(len(small.Data))}, ""); err != nil {
			t.Fatal(err)
		}
		if len(big.Data) <= len(small.Data) {
			t.Fatalf("the icons: %d %d", len(big.Data), len(small.Data))
		}
		if _, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{Icon: big}); !errors.Is(err, store.ErrTenantLimit) {
			t.Fatalf("an icon past the limit: %v", err)
		}
		if _, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{Icon: small}); err != nil {
			t.Fatalf("an icon within it: %v", err)
		}
		if now, _ := en.st.TenantBytes(ctx, tn.ID); now != used+int64(len(small.Data)) {
			t.Fatalf("the icon counted: %d", now)
		}
		if _, err := rel.SetEntryMeta(ctx, distAdmin, "acme", "", "publisher:enterest-ci", "geo", release.MetaWrite{Icon: big}); err != nil {
			t.Fatalf("a distributor's icon: %v", err)
		}
	})
}

// Spec 0038, regression: an intake's build is in flight between its insert and its release; a
// second intake meanwhile does not take the room it needs. Exactly one of the two is kept, and the
// tenant never holds more than its limit.
func TestTenantBytesLimitInFlight(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		p.publish(t, cpp("1.0"))
		tn, _ := p.st.GetTenant(ctx, "acme")
		used, _ := p.st.TenantBytes(ctx, tn.ID)
		one := int64(len(built(t, cpp("1.1"), "tresor_duckdb_cpp_init")))
		// room for one more body of about that size, not two
		if _, _, err := p.ten.SetTenantLimits(ctx, admin, "acme", map[string]int64{store.LimitBytes: used + one + one/2}, ""); err != nil {
			t.Fatal(err)
		}
		var second error
		release.SetAfterBuild(p.pubs, func(store.Build) {
			release.SetAfterBuild(p.pubs, nil)
			_, _, second = p.pubs.Add(ctx, p.pub, "acme", "staging", bytes.NewReader(built(t, cpp("1.2"), "tresor_duckdb_cpp_init")),
				publish("tresor", "1.2", "linux_amd64"))
		})
		_, _, first := p.pubs.Add(ctx, p.pub, "acme", "staging", bytes.NewReader(built(t, cpp("1.1"), "tresor_duckdb_cpp_init")),
			publish("tresor", "1.1", "linux_amd64"))
		if (first == nil) == (second == nil) {
			t.Fatalf("both or neither kept: %v, %v", first, second)
		}
		for _, err := range []error{first, second} {
			if err != nil && !errors.Is(err, store.ErrTenantLimit) {
				t.Fatalf("refused otherwise: %v", err)
			}
		}
		if now, _ := p.st.TenantBytes(ctx, tn.ID); now > used+one+one/2 {
			t.Fatalf("past the limit: %d > %d", now, used+one+one/2)
		}
	})
}

// Spec 0038, regression: a build the tenant no longer counts (unlanded, waiting for the collector)
// found again by an intake is checked as new bytes when it is kept again.
func TestTenantBytesLimitFoundAgain(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		b, _, err := p.catalogueOnly(t, cpp("1.0"))
		if err != nil {
			t.Fatal(err)
		}
		if err := p.st.Unland(ctx, []string{b.ID}); err != nil {
			t.Fatal(err)
		}
		tn, _ := p.st.GetTenant(ctx, "acme")
		if used, _ := p.st.TenantBytes(ctx, tn.ID); used != 0 {
			t.Fatalf("an unlanded build counted: %d", used)
		}
		if _, _, err := p.ten.SetTenantLimits(ctx, admin, "acme", map[string]int64{store.LimitBytes: 0}, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.catalogueOnly(t, cpp("1.0")); !errors.Is(err, store.ErrTenantLimit) {
			t.Fatalf("the same body landed again at a full tenant: %v", err)
		}
		if used, _ := p.st.TenantBytes(ctx, tn.ID); used != 0 {
			t.Fatalf("past the limit: %d", used)
		}
	})
}
