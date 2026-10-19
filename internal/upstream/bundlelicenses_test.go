package upstream_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// carriedNames lists a manifest's licences as source-less names.
func carriedNames(m *bundle.Manifest) []string {
	var out []string
	for _, s := range m.Sources {
		for _, l := range s.Licenses {
			out = append(out, l.Name)
		}
	}
	return out
}

// Spec 0021: a bundle carries the licences a site's plan asks for, when the exporter agrees, for
// the names whose files come; the site takes each into the first of its bundle-mode upstreams of
// the source that supplies the name, verified with that upstream's keys, never replacing an
// administrator's import and never revoking.
func TestBundleLicenses(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.repo.token = "T-sub"
			if err := os.WriteFile(g.tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			lic := newLicensor(t)
			for _, n := range []string{"tresor", "extra"} {
				g.repo.put("v2.0.0", "linux_amd64", n, signedWith(t, g.key, cpp("1.0", "v2.0.0"), lic.block, n+"_duckdb_cpp_init"))
			}
			g.put(t, "plain", "1.0", 2)
			sp := g.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "extra"}, store.UpstreamEntry{Name: "plain"})
			sp.Credential = "sub"
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			svc := &licenses.Service{Store: g.st, Authz: authz.ServerAdmin{}, Enabled: true, Log: slog.New(slog.DiscardHandler)}
			g.up.Licenses = svc
			now := g.st.Now().Unix()
			for _, n := range []string{"tresor", "extra"} {
				if _, err := svc.Import(ctx, admin, "acme", lic.sign(n, "pro", now-60, now+30*86400), licenses.Create); err != nil {
					t.Fatal(err)
				}
			}
			// the site: two bundle-mode upstreams of the source, in prod and in staging
			g.air(t, "tresor", "plain")
			must(t, func() error {
				_, err := g.ten.CreateChannel(ctx, admin, "site", "staging", store.ChannelSigned)
				return err
			})
			must(t, func() error {
				_, err := g.ten.SetChannelVersions(ctx, admin, "site", "staging", []string{"v2.0.0"}, nil)
				return err
			})
			if err := signer.GenerateKeyFile(filepath.Join(g.dir, "site2.pem")); err != nil {
				t.Fatal(err)
			}
			must(t, func() error { _, err := g.ks.Add(ctx, admin, "site", "staging", "file:site2.pem", true); return err })
			sp2 := g.bundleSpec("air2", "tresor")
			sp2.Prefix, sp2.Channel = g.repo.srv.URL, "staging"
			air2, err := g.up.Add(ctx, admin, "site", sp2)
			if err != nil {
				t.Fatal(err)
			}
			air, err := g.up.Get(ctx, admin, "site", "air")
			if err != nil {
				t.Fatal(err)
			}
			site, err := g.st.GetTenant(ctx, "site")
			if err != nil {
				t.Fatal(err)
			}
			stored := func() store.License {
				t.Helper()
				l, err := g.st.GetLicense(ctx, site.ID, "tresor")
				if err != nil {
					t.Fatalf("the site's licence: %v", err)
				}
				return l
			}
			plan := func(holder string) bundle.Plan {
				t.Helper()
				p, err := g.up.Plan(ctx, admin, "site", upstream.PlanTargets{Licenses: true, LicenseHolder: holder}, []string{g.fp})
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			importSite := func(data []byte, dryRun bool) upstream.ImportResult {
				t.Helper()
				res, err := g.up.Import(ctx, admin, "site", bytes.NewReader(data), upstream.ImportOptions{BundleKeys: []string{g.fp}, Holder: "test", DryRun: dryRun})
				if err != nil {
					t.Fatalf("import: %v", err)
				}
				return res
			}
			private := []string{g.repo.srv.URL}

			// a plan that asks for none carries none, even with -licenses
			res, data := g.export(t, g.plan(t), upstream.ExportOptions{Private: private, Licenses: true})
			if m, _, _ := g.open(t, data); len(carriedNames(m)) != 0 || m.Plan.Licenses || !strings.Contains(res.LicenseNote, "asks for no licences") {
				t.Fatalf("a plan without licences: %v %q", carriedNames(m), res.LicenseNote)
			}
			// the plan asks, the exporter does not agree
			res, data = g.export(t, plan(""), upstream.ExportOptions{Private: private})
			if m, _, _ := g.open(t, data); len(carriedNames(m)) != 0 || !m.Plan.Licenses || !strings.Contains(res.LicenseNote, "-licenses") {
				t.Fatalf("without -licenses: %v %q", carriedNames(m), res.LicenseNote)
			}
			// both: tresor's only; extra's files are not in the bundle, plain has no licence
			res, data = g.export(t, plan(""), upstream.ExportOptions{Private: private, Licenses: true})
			m, _, _ := g.open(t, data)
			if got := carriedNames(m); len(got) != 1 || got[0] != "tresor" || res.LicenseNote != "" {
				t.Fatalf("carried: %v %q", got, res.LicenseNote)
			}
			if ls := res.Sources[0].Licenses; len(ls) != 1 || ls[0].Name != "tresor" || ls[0].HolderID != "acme" || ls[0].Holder != "Acme" {
				t.Fatalf("what the exporter is shown: %+v", ls)
			}
			// the first target that supplies the name takes it: air, before air2
			ir := importSite(data, false)
			if l := ir.Licenses; l == nil || l.Counts["stored"] != 1 || l.Counts["refused"] != 0 {
				t.Fatalf("the import's licences: %+v", ir.Licenses)
			}
			if l := stored(); l.Source != store.LicenseUpstream || l.UpstreamID != air.ID {
				t.Fatalf("the stored licence: %s %s, not air %s", l.Source, l.UpstreamID, air.ID)
			}
			rec, err := g.up.GetImport(ctx, admin, "site", ir.Import.ID)
			if err != nil || !strings.Contains(rec.Counts, `"licenses":{"counts":{`) {
				t.Fatalf("the record: %s %v", rec.Counts, err)
			}
			if evs, err := g.st.ListEvents(ctx, site.ID, store.EventFilter{Kind: "bundle.import", Limit: 1}); err != nil || len(evs) == 0 ||
				!strings.Contains(evs[0].Data, `"licenses":{"counts":{`) {
				t.Fatalf("the event: %+v", evs)
			}
			// the site holds the files now: their licences still come, unchanged
			res, data = g.export(t, plan(""), upstream.ExportOptions{Private: private, Licenses: true})
			if outcomes(res)[bundle.Unchanged] == 0 || importSite(data, false).Licenses.Counts["unchanged"] != 1 {
				t.Fatalf("an unchanged bundle: %+v", res)
			}

			// a later licence: a dry run counts it and stores nothing; the import replaces
			later := lic.sign("tresor", "max", now, now+60*86400)
			if _, err := svc.Import(ctx, admin, "acme", later, 0); err != nil {
				t.Fatal(err)
			}
			_, data = g.export(t, plan(""), upstream.ExportOptions{Private: private, Licenses: true})
			if l := importSite(data, true).Licenses; l.Counts["replaced"] != 1 || stored().Token == later {
				t.Fatalf("a dry run: %+v", l)
			}
			if l := importSite(data, false).Licenses; l.Counts["replaced"] != 1 || stored().Token != later {
				t.Fatalf("the later licence: %+v", l)
			}

			// no revocation: a bundle without licences leaves the stored one
			_, data = g.export(t, g.plan(t), upstream.ExportOptions{Private: private})
			if ir := importSite(data, false); ir.Licenses != nil || stored().Token != later {
				t.Fatalf("a bundle without licences: %+v", ir.Licenses)
			}

			// an administrator's import at the site is never replaced by a bundle's
			own := lic.sign("tresor", "site", now, now+90*86400)
			if _, err := svc.Import(ctx, admin, "site", own, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Import(ctx, admin, "acme", lic.sign("tresor", "max", now, now+120*86400), 0); err != nil {
				t.Fatal(err)
			}
			_, data = g.export(t, plan(""), upstream.ExportOptions{Private: private, Licenses: true})
			if l := importSite(data, false).Licenses; l.Counts["refused"] != 1 || l.Refused[0].Rule != "superseded" || stored().Token != own {
				t.Fatalf("over an administrator's import: %+v", l)
			}

			// the holder the plan names: the exporter carries only that holder's
			res, data = g.export(t, plan("someone-else"), upstream.ExportOptions{Private: private, Licenses: true})
			if m, _, _ := g.open(t, data); len(carriedNames(m)) != 0 || m.Plan.LicenseHolder != "someone-else" || len(res.Sources[0].Licenses) != 0 {
				t.Fatalf("another holder: %v", carriedNames(m))
			}
			if _, err := g.up.Plan(ctx, admin, "site", upstream.PlanTargets{LicenseHolder: "x"}, []string{g.fp}); !errors.Is(err, bundle.ErrInvalid) {
				t.Fatalf("a holder without licences: %v", err)
			}

			// the site's own checks, as a carrier cannot pass them by the exporter
			if err := svc.Remove(ctx, admin, "site", "tresor", 0); err != nil {
				t.Fatal(err)
			}
			tok := lic.sign("tresor", "pro", now, now+86400)
			apply := func(holder string, ls ...licenses.BundleLicense) licenses.Result {
				t.Helper()
				r := svc.ApplyBundle(ctx, site, "test", holder, ls, false)
				if r.Error != "" {
					t.Fatal(r.Error)
				}
				return r
			}
			for _, c := range []struct {
				holder string
				bl     licenses.BundleLicense
				rule   string
			}{
				{"someone-else", licenses.BundleLicense{Name: "tresor", Token: tok, Upstreams: []store.Upstream{air}}, licenses.RuleHolder},
				{"", licenses.BundleLicense{Name: "plain", Token: tok, Upstreams: []store.Upstream{air}}, license.RuleExtMismatch},
				{"", licenses.BundleLicense{Name: "tresor", Token: tok}, licenses.RuleNoTarget},
				{"", licenses.BundleLicense{Name: "extra", Token: lic.sign("extra", "pro", now, now+86400), Upstreams: []store.Upstream{air}}, "not_supplied"},
			} {
				if r := apply(c.holder, c.bl); r.Counts["refused"] != 1 || r.Refused[0].Rule != c.rule {
					t.Errorf("%s: %+v", c.rule, r)
				}
			}
			// one target per licence, in the import's order
			if r := apply("acme", licenses.BundleLicense{Name: "tresor", Token: tok, Upstreams: []store.Upstream{air2, air}}); r.Counts["stored"] != 1 ||
				stored().UpstreamID != air2.ID {
				t.Fatalf("the first target: %+v %s", r, stored().UpstreamID)
			}
			// licensing off at the site
			svc.Enabled = false
			if r := apply("", licenses.BundleLicense{Name: "tresor", Token: tok, Upstreams: []store.Upstream{air}}); r.Refused[0].Rule != licenses.RuleOff {
				t.Fatalf("licensing off: %+v", r)
			}
		})
	}
}
