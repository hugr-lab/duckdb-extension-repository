package upstream_test

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0035 phase 2: a bundle's import into a two-platform target whose releases are public warns
// once per import: the licence stored into the target's entry with them, then a later version of
// both platforms (each its own transaction) into the licensed entry.
func TestBundleLicensePublicWarning(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.repo.token = "T-sub"
			if err := os.WriteFile(g.tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			g.rel.MayPublish = func(string, string) bool { return true } // the bundle's releases keep their public visibility
			lic := newLicensor(t)
			platforms := []string{"linux_amd64", "linux_amd64_musl"} // both x86-64 ELF
			put := func(ver string) {
				for _, pl := range platforms {
					m := cpp(ver, "v2.0.0")
					m.Platform = pl
					g.repo.put("v2.0.0", pl, "tresor", signedWith(t, g.key, m, lic.block, "tresor_duckdb_cpp_init"))
				}
			}
			put("1.0")
			sp := g.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Credential, sp.Platforms = "sub", platforms
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			if r := g.sync(t, false); r.Counts[store.CellReleased] != 2 {
				t.Fatalf("the source's run: %+v", r)
			}
			svc := &licenses.Service{Store: g.st, Authz: authz.ServerAdmin{}, Enabled: true, Log: slog.New(slog.DiscardHandler)}
			g.up.Licenses = svc
			now := g.st.Now().Unix()
			if _, err := svc.Import(ctx, admin, "acme", "", "upstream:acme-repo", lic.sign("tresor", "pro", now-60, now+30*86400), licenses.Create); err != nil {
				t.Fatal(err)
			}
			air := g.bundleSpec("air", "tresor")
			air.Prefix, air.Platforms = g.repo.srv.URL, platforms
			if _, err := g.up.Add(ctx, admin, "site", air); err != nil {
				t.Fatal(err)
			}
			site, err := g.st.GetTenant(ctx, "site")
			if err != nil {
				t.Fatal(err)
			}
			warnings := func() []store.Event {
				t.Helper()
				evs, err := g.st.ListEvents(ctx, site.ID, store.EventFilter{Kind: string(store.LicenseWarningKind), Limit: 100})
				if err != nil {
					t.Fatal(err)
				}
				return evs
			}
			bundled := func() []byte {
				t.Helper()
				p, err := g.up.Plan(ctx, admin, "site", upstream.PlanTargets{Licenses: true}, []string{g.fp})
				if err != nil {
					t.Fatal(err)
				}
				_, data := g.export(t, p, upstream.ExportOptions{Private: []string{g.repo.srv.URL}, Licenses: true})
				return data
			}
			importSite := func(data []byte) {
				t.Helper()
				if _, err := g.up.Import(ctx, admin, "site", bytes.NewReader(data), upstream.ImportOptions{BundleKeys: []string{g.fp}, Holder: "test"}); err != nil {
					t.Fatal(err)
				}
			}

			// the first import: two public releases and the licence into air's entry, one warning
			importSite(bundled())
			if rs, err := g.rel.List(ctx, admin, "site", "prod", "tresor"); err != nil || len(rs) != 2 || rs[0].Visibility != store.Public {
				t.Fatalf("the site's releases: %+v %v", rs, err)
			}
			if evs := warnings(); len(evs) != 1 || !strings.Contains(evs[0].Data, `"source":"upstream:air"`) || evs[0].Subject != "channel:prod/ext:tresor" {
				t.Fatalf("the first import: %+v", evs)
			}
			// a later version of both platforms into the licensed entry: one more, by the releases
			put("1.1")
			g.sync(t, false)
			importSite(bundled())
			if rs, err := g.rel.List(ctx, admin, "site", "prod", "tresor"); err != nil || len(rs) != 4 {
				t.Fatalf("the site's releases after 1.1: %+v %v", rs, err)
			}
			if evs := warnings(); len(evs) != 2 || !strings.Contains(evs[0].Data, `"act":"upstream.release"`) {
				t.Fatalf("the second import: %+v", evs)
			}
		})
	}
}
