package release_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// catalogueOnly publishes into the tenant's catalogue as the publisher ci.
func (p placing) catalogueOnly(t *testing.T, m extfile.Metadata, exports ...string) (store.Build, bool, error) {
	t.Helper()
	if len(exports) == 0 {
		exports = []string{"tresor_duckdb_cpp_init"}
	}
	return p.pubs.AddToCatalogue(ctx, p.pub, "acme", bytes.NewReader(built(t, m, exports...)), publish("tresor", m.ExtensionVersion, m.Platform))
}

// Spec 0033: a version is immutable within its source in the whole tenant: another body of a version
// the source published into one channel is refused in another, and in the catalogue.
func TestCatalogueSlot(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		p.publish(t, cpp("1.0"))
		other := built(t, cpp("1.0"), "tresor_duckdb_cpp_init", "other")
		if _, _, err := p.pubs.Add(ctx, p.pub, "acme", "prod", bytes.NewReader(other), publish("tresor", "1.0", "linux_amd64")); !errors.Is(err, release.ErrSlot) {
			t.Fatalf("another body of 1.0 in prod: %v", err)
		}
		if _, _, err := p.catalogueOnly(t, cpp("1.0"), "tresor_duckdb_cpp_init", "other"); !errors.Is(err, release.ErrSlot) {
			t.Fatalf("another body of 1.0 in the catalogue: %v", err)
		}
		// the tenant's own source is another source: its 1.0 may differ
		if _, _, err := en.addTo(t, "prod", other, release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatalf("the tenant's 1.0: %v", err)
		}
		// a version once released stays the source's after its release is purged and its build
		// collected: the purged slot is its tombstone, and its entry is not purged
		p.publish(t, cpp("2.0"))
		ch, _ := p.st.GetChannel(ctx, "acme", "staging")
		rs, _ := p.st.ListReleases(ctx, ch.ID, "tresor")
		var r2 store.Candidate
		for _, r := range rs {
			if r.ExtVersion == "2.0" {
				r2 = r
			}
		}
		if _, err := p.rel.Apply(ctx, admin, "acme", "staging", "tresor", r2.ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := p.rel.Purge(ctx, admin, "acme", "staging", "tresor", r2.ID, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := p.rel.PurgeEntry(ctx, admin, "acme", r2.BuildID); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("the entry of a version once released, purged: %v", err)
		}
		if gone, err := p.st.DeleteBuild(ctx, r2.BuildID, time.Now().Add(48*time.Hour)); err != nil || !gone {
			t.Fatalf("collected: %v %v", gone, err)
		}
		if _, _, err := p.catalogueOnly(t, cpp("2.0"), "tresor_duckdb_cpp_init", "again"); !errors.Is(err, release.ErrSlot) {
			t.Fatalf("another body of a version once released: %v", err)
		}
	})
}

// Spec 0033: publishing into the catalogue only lands the build, placed nowhere and kept from the
// collector; the same body again is the build; a DuckDB version the server does not know is refused;
// a purge frees the slot; the catalogue keeps a source's KeepLanded newest landed versions of a name.
func TestCatalogueOnly(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		b, existed, err := p.catalogueOnly(t, cpp("1.0"))
		if err != nil || existed || b.Source != "publisher:"+p.ci.ID || b.Visibility != store.Public {
			t.Fatalf("published: %+v %v %v", b, existed, err)
		}
		if _, existed, err := p.catalogueOnly(t, cpp("1.0")); err != nil || !existed {
			t.Fatalf("again: %v %v", existed, err)
		}
		unknown := cpp("1.1")
		unknown.DuckDBVersion = "v9.9.9"
		if _, _, err := p.catalogueOnly(t, unknown); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an unknown DuckDB version: %v", err)
		}
		ch, _ := p.st.GetChannel(ctx, "acme", "prod")
		if rs, _ := p.st.ListReleases(ctx, ch.ID, "tresor"); len(rs) != 0 {
			t.Fatalf("placed: %+v", rs)
		}
		if dead, err := p.st.DeadBuilds(ctx, "default", time.Now().Add(time.Hour), "", 100); err != nil || len(dead) != 0 {
			t.Fatalf("a landed build collectable: %v %v", dead, err)
		}
		// purged: its slot is free, another body of 1.0 is taken
		if _, err := p.rel.PurgeEntry(ctx, admin, "acme", b.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.catalogueOnly(t, cpp("1.0"), "tresor_duckdb_cpp_init", "fixed"); err != nil {
			t.Fatalf("1.0 again after the purge: %v", err)
		}
		// a released build is not purged
		p.publish(t, cpp("2.0"))
		bs, _ := p.st.SourceBuilds(ctx, ch.TenantID, "publisher:"+p.ci.ID, "tresor")
		for _, x := range bs {
			if x.ExtVersion == "2.0" {
				if _, err := p.rel.PurgeEntry(ctx, admin, "acme", x.ID); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("a released build purged: %v", err)
				}
			}
		}
		// the newest KeepLanded landed versions of a (name, platform, slot) stay; older ones are left to
		// the collector; another slot's are counted apart
		must(t, func() error {
			_, _, err := p.ten.AddVersion(ctx, admin, "v2.1.0", "release", []string{"v1.5.6", "v2.0.0"}, tenants.CoreExtensions{Names: []string{"json"}})
			return err
		})
		for _, v := range []string{"1.1", "1.2", "1.3"} {
			if _, _, err := p.catalogueOnly(t, cpp(v)); err != nil {
				t.Fatal(err)
			}
		}
		on21 := cpp("0.1")
		on21.DuckDBVersion = "v2.1.0"
		if _, _, err := p.catalogueOnly(t, on21); err != nil {
			t.Fatal(err)
		}
		n, err := p.rel.TrimLanded(ctx)
		if err != nil || n != 1 {
			t.Fatalf("trimmed: %d %v", n, err)
		}
		dead, err := p.st.DeadBuilds(ctx, "default", time.Now().Add(48*time.Hour), "", 100)
		if err != nil || len(dead) != 1 {
			t.Fatalf("collectable after the trim: %v %v", dead, err)
		}
		bs, _ = p.st.SourceBuilds(ctx, ch.TenantID, "publisher:"+p.ci.ID, "tresor")
		for _, x := range bs {
			if x.ID == dead[0] && x.ExtVersion != "1.0" {
				t.Fatalf("trimmed %s, not the oldest", x.ExtVersion)
			}
		}
		if gone, err := p.st.DeleteBuild(ctx, dead[0], time.Now().Add(48*time.Hour)); err != nil || !gone {
			t.Fatalf("collected: %v %v", gone, err)
		}
	})
}

// Spec 0033: a source holds at most catalogue.max_entries builds: a new one beyond is refused, the
// same body again is not new.
func TestCatalogueFull(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		p.pubs.MaxEntries = 2
		for _, v := range []string{"1.0", "1.1"} {
			if _, _, err := p.catalogueOnly(t, cpp(v)); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := p.catalogueOnly(t, cpp("1.2")); !errors.Is(err, store.ErrCatalogueFull) {
			t.Fatalf("beyond the bound: %v", err)
		}
		if _, existed, err := p.catalogueOnly(t, cpp("1.1")); err != nil || !existed {
			t.Fatalf("a build it holds: %v %v", existed, err)
		}
		// an administrator's add is not bounded
		if _, _, err := p.rel.AddToCatalogue(ctx, admin, "acme", bytes.NewReader(built(t, cpp("1.2"), "tresor_duckdb_cpp_init")),
			release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatalf("an administrator's add: %v", err)
		}
		// the bytes bound
		p.pubs.MaxEntries, p.pubs.MaxBytes = 0, 1
		if _, _, err := p.catalogueOnly(t, cpp("1.3")); !errors.Is(err, store.ErrCatalogueFull) {
			t.Fatalf("beyond the bytes: %v", err)
		}
	})
}
