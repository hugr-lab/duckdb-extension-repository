package upstream_test

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// keysOf lists the tenant's live licence keys as name/kid/source lines.
func (en *env) keysOf(t *testing.T) []string {
	t.Helper()
	ks, err := en.st.TenantLicenseKeys(ctx, en.tenantID(t), pkgtype.DuckDBExtension, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, k := range ks {
		out = append(out, fmt.Sprintf("%s %v", k.Name, k.Sources))
	}
	return out
}

// Spec 0021: a repository upstream's mirror run and pull-through record the keys its bodies
// declare; a run that finds the body in its slot records them too; removing the upstream drops them.
func TestUpstreamLicenseKeys(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		lic := newLicensor(t)
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signedWith(t, en.key, cpp("1.0", "v2.0.0"), lic.block, "tresor_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "pulled", signedWith(t, en.key, cpp("1.0", "v2.0.0"), lic.block, "pulled_duckdb_cpp_init"))
		en.repo.put("v2.0.0", "linux_amd64", "plain", signed(t, en.key, cpp("1.0", "v2.0.0"), 3, "plain_duckdb_cpp_init"))
		sp := en.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "plain"})
		if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 2 {
			t.Fatalf("the run: %+v", r)
		}
		if got := fmt.Sprint(en.keysOf(t)); got != "[tresor [{upstream acme-repo}]]" {
			t.Fatalf("after the mirror run: %s", got)
		}

		// pull-through
		u, err := en.up.Add(ctx, admin, "acme", en.pullSpec(store.UpstreamEntry{Name: "pulled"}))
		if err != nil {
			t.Fatal(err)
		}
		p := en.puller()
		p.Offer(release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "pulled"})
		p.Drain(ctx)
		if got := fmt.Sprint(en.keysOf(t)); got != "[pulled [{upstream pull}] tresor [{upstream acme-repo}]]" {
			t.Fatalf("after the pull-through: %s", got)
		}

		// removing the mirror drops its keys; added again, a run that finds the body in its slot (the
		// removed upstream's releases are the new one's) records them for the new upstream
		if err := en.up.Remove(ctx, admin, "acme", sp.Name, 0); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(en.keysOf(t)); got != "[pulled [{upstream pull}]]" {
			t.Fatalf("after removing the upstream: %s", got)
		}
		if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellUnchanged] != 2 {
			t.Fatalf("the run again: %+v", r)
		}
		if got := fmt.Sprint(en.keysOf(t)); got != "[pulled [{upstream pull}] tresor [{upstream acme-repo}]]" {
			t.Fatalf("after the run again: %s", got)
		}
	})
}

// Spec 0021: the relay counts only the keys its upstream brought; a licence it relays that no live
// build of the upstream's declares any more is refused and kept, not revoked.
func TestRelayLicenseKeys(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en, tokenFile := privateEnv(t, e)
			en.repo.token = "T-sub"
			if err := os.WriteFile(tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			lic := newLicensor(t)
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signedWith(t, en.key, cpp("1.0", "v2.0.0"), lic.block, "tresor_duckdb_cpp_init"))
			en.repo.put("v2.0.0", "linux_amd64", "plain", signed(t, en.key, cpp("1.0", "v2.0.0"), 2, "plain_duckdb_cpp_init"))
			sp := en.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "plain"})
			sp.Credential = "sub"
			u, err := en.up.Add(ctx, admin, "acme", sp)
			if err != nil {
				t.Fatal(err)
			}
			en.sync(t, false)
			svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Fetch: en.up.Fetch,
				Credentials: en.up.Credentials, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
			// plain's key comes from a publisher, not from the upstream
			if _, err := storetest.LicenseKey(ctx, en.st, en.tenantID(t), "plain", store.KeySourcePublisher, "plain-ci", &lic.key.PublicKey); err != nil {
				t.Fatal(err)
			}
			now := en.st.Now().Unix()
			en.answer(t, lic.sign("tresor", "pro", now-60, now+30*86400), lic.sign("plain", "pro", now-60, now+30*86400))
			res, err := svc.RunUpstream(ctx, u.ID)
			if err != nil || res.Counts["stored"] != 1 || res.Counts["refused"] != 1 || res.Refused[0].Name != "plain" ||
				res.Refused[0].Rule != license.RuleKeyUntrusted {
				t.Fatalf("the relay: %+v %v", res, err)
			}
			// a refused key counts nowhere: the relayed licence is refused and stays stored, and an
			// import is refused
			svc.Refuse = map[string]bool{vectorsKid(lic): true}
			res, err = svc.RunUpstream(ctx, u.ID)
			if err != nil || res.Counts["removed"] != 0 || res.Counts["refused"] != 2 || res.Refused[0].Rule != license.RuleKeyUntrusted {
				t.Fatalf("the relay with the key refused: %+v %v", res, err)
			}
			if _, err := nameLicense(ctx, en.st, en.tenantID(t), pkgtype.DuckDBExtension, "tresor"); err != nil {
				t.Fatalf("the relayed licence was removed: %v", err)
			}
			if _, err := svc.Import(ctx, admin, "acme", "", lic.sign("plain", "max", now, now+86400), licenses.Create); !errors.Is(err, license.ErrUntrustedKey) {
				t.Fatalf("an import with a refused key: %v", err)
			}
		})
	}
}

func vectorsKid(l *licensor) string { return extfile.Fingerprint(&l.key.PublicKey) }
