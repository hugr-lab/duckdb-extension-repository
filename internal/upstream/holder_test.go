package upstream_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0028: with an expected holder, an import of another holder's licence is refused, by the rule
// holder_mismatch, naming both holders; with none, it is taken as before.
func TestLicenseImportHolder(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			lic := newLicensor(t)
			svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Log: slog.New(slog.DiscardHandler)}
			now := en.st.Now().Unix()
			day := int64(24 * 3600)
			for _, name := range []string{"tresor", "geo"} {
				if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(signedWith(t, nil, cpp("1.0", "v2.0.0"), lic.block,
					name+"_duckdb_cpp_init")), release.AddOptions{Name: name}); err != nil {
					t.Fatal(err)
				}
				if _, err := storetest.LicenseKey(ctx, en.st, en.tenantID(t), name, store.KeySourcePublisher, "p", &lic.key.PublicKey); err != nil {
					t.Fatal(err)
				}
			}
			// no holder: any holder's licence, as before
			if _, err := svc.Import(ctx, admin, "acme", "", lic.signFor("org-2", "geo", "pro", now-60, now+day), licenses.Create); err != nil {
				t.Fatalf("no expected holder: %v", err)
			}
			if _, err := en.ten.SetTenantHolder(ctx, admin, "acme", "org-1", 0); err == nil {
				t.Fatal("the holder set over another holder's licence")
			}
			if err := svc.Remove(ctx, admin, "acme", "", "geo", "", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := en.ten.SetTenantHolder(ctx, admin, "acme", "org-1", 0); err != nil {
				t.Fatal(err)
			}
			_, err := svc.Import(ctx, admin, "acme", "", lic.signFor("org-2", "tresor", "pro", now-60, now+day), licenses.Create)
			var le *license.Error
			if !errors.Is(err, licenses.ErrRefused) || !errors.As(err, &le) || le.Rule != licenses.RuleHolder ||
				!strings.Contains(err.Error(), `"org-1"`) || !strings.Contains(err.Error(), `"org-2"`) {
				t.Fatalf("another holder's licence: %v", err)
			}
			if _, err := nameLicense(ctx, en.st, en.tenantID(t), pkgtype.DuckDBExtension, "tresor"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a refused import was stored: %v", err)
			}
			if n, _ := store.CountRaw(ctx, en.st, "events", "kind = ?", "license.import"); n != 1 {
				t.Fatalf("license.import events: %d", n)
			}
			v, err := svc.Import(ctx, admin, "acme", "", lic.signFor("org-1", "tresor", "pro", now-60, now+day), licenses.Create)
			if err != nil || v.HolderID != "org-1" {
				t.Fatalf("the holder's licence: %+v %v", v, err)
			}
			// a replacement of another holder is refused too, and the stored one stays
			if _, err := svc.Import(ctx, admin, "acme", "", lic.signFor("org-2", "tresor", "max", now, now+2*day), v.Version); !errors.As(err, &le) ||
				le.Rule != licenses.RuleHolder {
				t.Fatalf("a replacement of another holder: %v", err)
			}
			if l, err := nameLicense(ctx, en.st, en.tenantID(t), pkgtype.DuckDBExtension, "tresor"); err != nil || l.HolderID != "org-1" || l.Version != v.Version {
				t.Fatalf("after a refused replacement: %+v %v", l, err)
			}
		})
	}
}

// Spec 0028: with an expected holder, the relay refuses a licence of another holder (holder_mismatch,
// counted in license.sync) and stores the others.
func TestLicenseRelayHolder(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en, tokenFile := privateEnv(t, e)
			en.repo.token = "T-sub"
			if err := os.WriteFile(tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			lic := newLicensor(t)
			for _, name := range []string{"tresor", "geo"} {
				en.repo.put("v2.0.0", "linux_amd64", name, signedWith(t, en.key, cpp("1.0", "v2.0.0"), lic.block, name+"_duckdb_cpp_init"))
			}
			sp := en.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "geo"})
			sp.Credential = "sub"
			u, err := en.up.Add(ctx, admin, "acme", sp)
			if err != nil {
				t.Fatal(err)
			}
			if r := en.sync(t, false); r.Counts[store.CellReleased] != 2 {
				t.Fatalf("a run: %+v", r)
			}
			svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Fetch: en.up.Fetch,
				Credentials: en.up.Credentials, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
			now := en.st.Now().Unix()
			day := int64(24 * 3600)
			run := func() licenses.Result {
				t.Helper()
				res, err := svc.RunUpstream(ctx, u.ID)
				if err != nil || res.Error != "" {
					t.Fatalf("a relay run: %+v %v", res, err)
				}
				return res
			}
			if _, err := en.ten.SetTenantHolder(ctx, admin, "acme", "org-1", 0); err != nil {
				t.Fatal(err)
			}
			first := lic.signFor("org-1", "tresor", "pro", now-60, now+30*day)
			en.answer(t, first, lic.signFor("org-2", "geo", "pro", now-60, now+30*day))
			res := run()
			if res.Counts["stored"] != 1 || res.Counts["refused"] != 1 || res.Refused[0].Rule != licenses.RuleHolder || res.Refused[0].Name != "geo" ||
				!strings.Contains(res.Refused[0].Reason, `"org-2"`) {
				t.Fatalf("a relay with another holder's licence: %+v", res)
			}
			if _, err := nameLicense(ctx, en.st, en.tenantID(t), pkgtype.DuckDBExtension, "geo"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("another holder's licence was relayed: %v", err)
			}
			if l, err := nameLicense(ctx, en.st, en.tenantID(t), pkgtype.DuckDBExtension, "tresor"); err != nil || l.HolderID != "org-1" {
				t.Fatalf("the holder's licence: %+v %v", l, err)
			}
			evs, err := en.st.ListEvents(ctx, en.tenantID(t), store.EventFilter{Kind: "license.sync", Limit: 10})
			if err != nil || len(evs) == 0 || !strings.Contains(evs[0].Data, `"refused":1`) || !strings.Contains(evs[0].Data, licenses.RuleHolder) {
				t.Fatalf("license.sync: %+v %v", evs, err)
			}
			// a later answer naming another holder for a stored name: refused, and the stored one stays
			en.answer(t, lic.signFor("org-2", "tresor", "max", now, now+60*day))
			if res := run(); res.Counts["refused"] != 1 || res.Refused[0].Rule != licenses.RuleHolder || res.Counts["removed"] != 0 {
				t.Fatalf("another holder's replacement: %+v", res)
			}
			if l, err := nameLicense(ctx, en.st, en.tenantID(t), pkgtype.DuckDBExtension, "tresor"); err != nil || l.Plan != "pro" {
				t.Fatalf("after another holder's replacement: %+v %v", l, err)
			}
			// with the holder cleared, the relay takes any holder's, as before
			if _, err := en.ten.SetTenantHolder(ctx, admin, "acme", "", 0); err != nil {
				t.Fatal(err)
			}
			en.answer(t, first, lic.signFor("org-2", "geo", "pro", now-60, now+30*day))
			if res := run(); res.Counts["refused"] != 0 || res.Counts["stored"] != 1 || res.Counts["unchanged"] != 1 {
				t.Fatalf("no expected holder: %+v", res)
			}
		})
	}
}
