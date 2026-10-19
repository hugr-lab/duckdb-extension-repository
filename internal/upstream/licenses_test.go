package upstream_test

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// licensor signs licences with the vectors' test-only key, as an issuer would; block is the key
// block its builds carry (spec 0021).
type licensor struct {
	key   *rsa.PrivateKey
	block []byte
	n     int
}

func newLicensor(t *testing.T) *licensor {
	t.Helper()
	k, err := vectors.LoadKey("../license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}
	pemKey, err := extfile.MarshalPublicKeyPEM(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	block, err := extfile.EncodeLicenseKeys(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	return &licensor{key: k, block: block}
}

// sign is a licence for ext at plan, issued at iat and expiring at exp (seconds).
func (l *licensor) sign(ext, plan string, iat, exp int64) string {
	return l.signFor("acme", ext, plan, iat, exp)
}

// signFor signs a licence of the holder sub.
func (l *licensor) signFor(sub, ext, plan string, iat, exp int64) string {
	l.n++
	p := fmt.Sprintf(`{"v":1,"iss":"https://issuer.example","jti":"lic-%d","sub":%q,"holder":"Acme","ext":%q,"plan":%q,"iat":%d,"exp":%d}`,
		l.n, sub, ext, plan, iat, exp)
	return vectors.Sign(l.key, vectors.Header(l.key), p)
}

func (en *env) answer(t *testing.T, tokens ...string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"licenses": append([]string{}, tokens...)})
	if err != nil {
		t.Fatal(err)
	}
	en.repo.putRaw("/-/licenses", b)
}

// Spec 0012 phase 1a: the relay of an upstream's licences, its rules, and what removes them.
func TestLicenseRelay(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en, tokenFile := privateEnv(t, e)
			en.repo.token = "T-sub"
			if err := os.WriteFile(tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			lic := newLicensor(t)
			// the build declares the licensor's key: the mirror run records it for the upstream (spec 0021)
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signedWith(t, en.key, cpp("1.0", "v2.0.0"), lic.block, "tresor_duckdb_cpp_init"))
			sp := en.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Credential = "sub"
			u, err := en.up.Add(ctx, admin, "acme", sp)
			if err != nil {
				t.Fatal(err)
			}
			if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
				t.Fatalf("a run: %+v", r)
			}
			svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Fetch: en.up.Fetch,
				Credentials: en.up.Credentials, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
			now := en.st.Now().Unix()
			day := int64(24 * 3600)
			due := func() bool {
				ids, err := en.st.DueLicenseSyncs(ctx, en.st.Now(), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				return slices.Contains(ids, u.ID)
			}
			run := func() licenses.Result {
				t.Helper()
				res, err := svc.RunUpstream(ctx, u.ID)
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			stored := func() (store.License, bool) {
				l, err := en.st.GetLicense(ctx, en.tenantID(t), "tresor")
				if errors.Is(err, store.ErrNotFound) {
					return l, false
				}
				if err != nil {
					t.Fatal(err)
				}
				return l, true
			}
			if !due() {
				t.Fatal("a credentialed upstream that never relayed is not due")
			}

			// stored for a name it supplies; refused for another name and for a broken token
			first := lic.sign("tresor", "pro", now-60, now+30*day)
			en.answer(t, first, lic.sign("httpfs", "pro", now-60, now+30*day), "not.a.licence")
			res := run()
			if res.Error != "" || res.Counts["received"] != 3 || res.Counts["stored"] != 1 || res.Counts["refused"] != 2 {
				t.Fatalf("first relay: %+v", res)
			}
			rules := []string{res.Refused[0].Rule, res.Refused[1].Rule}
			slices.Sort(rules)
			if !slices.Equal(rules, []string{"base64", "not_supplied"}) {
				t.Fatalf("refusals: %+v", res.Refused)
			}
			if a := en.repo.authorizations(); a[len(a)-1] != "Bearer T-sub" {
				t.Fatalf("the relay's Authorization: %q", a[len(a)-1])
			}
			l, ok := stored()
			if !ok || l.Source != store.LicenseUpstream || l.UpstreamID != u.ID || l.Visibility != store.Private || l.Plan != "pro" {
				t.Fatalf("stored: %+v", l)
			}
			if due() {
				t.Fatal("due right after a run")
			}
			if run().Counts["unchanged"] != 1 {
				t.Fatal("the same answer again is not unchanged")
			}

			// a changed plan replaces it, keeping its visibility
			if _, err := svc.SetVisibility(ctx, admin, "acme", "tresor", store.Public, 0); err != nil {
				t.Fatal(err)
			}
			en.answer(t, lic.sign("tresor", "enterprise", now-30, now+30*day))
			if res := run(); res.Counts["replaced"] != 1 {
				t.Fatalf("an upgrade: %+v", res)
			}
			if l, _ := stored(); l.Plan != "enterprise" || l.Visibility != store.Public {
				t.Fatalf("after the upgrade: %+v", l)
			}

			// a 404 and a refused token change nothing; an empty list revokes
			en.repo.mu.Lock()
			delete(en.repo.files, "/-/licenses")
			en.repo.mu.Unlock()
			if res := run(); res.Error != "" || len(res.Counts) != 0 {
				t.Fatalf("a 404: %+v", res)
			}
			en.answer(t)
			_ = os.WriteFile(tokenFile, []byte("other"), 0o600)
			if res := run(); res.Error == "" {
				t.Fatalf("a refused token: %+v", res)
			}
			if _, ok := stored(); !ok {
				t.Fatal("a failed relay removed the licence")
			}
			_ = os.WriteFile(tokenFile, []byte("T-sub"), 0o600)
			if res := run(); res.Counts["removed"] != 1 {
				t.Fatalf("revocation: %+v", res)
			}
			if _, ok := stored(); ok {
				t.Fatal("a revoked licence is still stored")
			}

			// an import, and the relay against it: the later expiry wins, an earlier one is refused
			imported := lic.sign("tresor", "pro", now-10, now+60*day)
			if _, err := svc.Import(ctx, admin, "acme", imported, licenses.Create); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Import(ctx, admin, "acme", imported, licenses.Create); !errors.Is(err, licenses.ErrExists) {
				t.Fatalf("an import over a stored licence without If-Match: %v", err)
			}
			en.answer(t, lic.sign("tresor", "pro", now, now+30*day))
			if res := run(); res.Counts["refused"] != 1 || res.Refused[0].Rule != "superseded" {
				t.Fatalf("a shorter relayed licence: %+v", res)
			}
			en.answer(t, lic.sign("tresor", "pro", now, now+90*day))
			if res := run(); res.Counts["replaced"] != 1 {
				t.Fatalf("a longer relayed licence: %+v", res)
			}

			// a requested run is due again; a paused upstream is not
			if err := svc.Sync(ctx, admin, "acme", u.Name); err != nil {
				t.Fatal(err)
			}
			if !due() {
				t.Fatal("a requested relay is not due")
			}
			run()
			if due() {
				t.Fatal("a request survived its run")
			}

			// a promotion keeps the name supplied: the build is the upstream's, whatever its mirror
			// channel holds now
			rs := en.releases(t, "tresor")
			if _, _, err := en.rel.Promote(ctx, admin, "acme", "other", "tresor", release.PromoteOptions{From: "prod", Release: rs[0].ID}); err != nil {
				t.Fatal(err)
			}
			if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", rs[0].ID, release.Yank, 0); err != nil {
				t.Fatal(err)
			}
			if res := run(); res.Counts["unchanged"] != 1 || res.Counts["refused"] != 0 {
				t.Fatalf("after the mirror's copy was yanked, the promoted one supplies it: %+v", res)
			}

			// a suspended tenant's relay is not due
			if _, err := en.ten.SetTenantState(ctx, admin, "acme", store.TenantSuspended, 0); err != nil {
				t.Fatal(err)
			}
			if err := svc.Sync(ctx, admin, "acme", u.Name); err != nil {
				t.Fatal(err)
			}
			if due() {
				t.Fatal("a suspended tenant's relay is due")
			}
			if _, err := en.ten.SetTenantState(ctx, admin, "acme", store.TenantActive, 0); err != nil {
				t.Fatal(err)
			}
			// a paused upstream takes no request
			if _, err := en.up.Set(ctx, admin, "acme", u.Name, "", store.UpstreamPaused, 0); err != nil {
				t.Fatal(err)
			}
			if err := svc.Sync(ctx, admin, "acme", u.Name); !errors.Is(err, licenses.ErrPaused) {
				t.Fatalf("a paused upstream's sync: %v", err)
			}
			if _, err := en.up.Set(ctx, admin, "acme", u.Name, "", store.UpstreamActive, 0); err != nil {
				t.Fatal(err)
			}

			// removing the upstream removes what it relayed, recorded
			if err := en.up.Remove(ctx, admin, "acme", u.Name, 0); err != nil {
				t.Fatal(err)
			}
			if _, ok := stored(); ok {
				t.Fatal("the removed upstream's licence is still stored")
			}
			evs, err := en.st.ListEvents(ctx, en.tenantID(t), store.EventFilter{Kind: "license.remove", Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			reasons := map[string]bool{}
			for _, ev := range evs {
				var d map[string]any
				_ = json.Unmarshal([]byte(ev.Data), &d)
				reasons[fmt.Sprint(d["reason"])] = true
			}
			if !reasons["revoked"] || !reasons["upstream removed"] {
				t.Fatalf("license.remove reasons: %v", reasons)
			}

			// an upstream added again under the same name supplies nothing it did not bring
			if u, err = en.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			en.answer(t, lic.sign("tresor", "pro", now, now+30*day))
			if res := run(); res.Counts["refused"] != 1 || res.Refused[0].Rule != "not_supplied" {
				t.Fatalf("a re-added upstream: %+v", res)
			}
		})
	}
}

// Spec 0012: what an import takes, and licences past their grace removed.
func TestLicenseImport(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			lic := newLicensor(t)
			svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Log: slog.New(slog.DiscardHandler)}
			now := en.st.Now().Unix()
			day := int64(24 * 3600)
			ok := lic.sign("tresor", "pro", now-60, now+day)
			off := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}}
			if _, err := off.Import(ctx, admin, "acme", ok, licenses.Create); !errors.Is(err, licenses.ErrOff) {
				t.Fatalf("licensing off: %v", err)
			}
			// spec 0021: a name with no live build declaring a key takes no licence
			if _, err := svc.Import(ctx, admin, "acme", ok, licenses.Create); !errors.Is(err, license.ErrUntrustedKey) ||
				!strings.Contains(err.Error(), "no live build of tresor declares a licence key") {
				t.Fatalf("no keys: %v", err)
			}
			if _, _, err := en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(signedWith(t, nil, cpp("1.0", "v2.0.0"), lic.block,
				"tresor_duckdb_cpp_init")), release.AddOptions{Name: "tresor"}); err != nil {
				t.Fatal(err)
			}
			// an administrator's add is no trusted source: still no key
			if _, err := svc.Import(ctx, admin, "acme", ok, licenses.Create); !errors.Is(err, license.ErrUntrustedKey) {
				t.Fatalf("an administrator's add recorded keys: %v", err)
			}
			if _, err := storetest.LicenseKey(ctx, en.st, en.tenantID(t), "tresor", store.KeySourcePublisher, "p", &lic.key.PublicKey); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Import(ctx, admin, "acme", lic.sign("tresor", "pro", now-40*day, now-30*day), licenses.Create); !errors.Is(err, licenses.ErrNotLive) {
				t.Fatalf("an expired licence: %v", err)
			}
			swapped := ok[:strings.LastIndex(ok, ".")] + lic.sign("tresor", "pro", now, now+day)[strings.LastIndex(ok, "."):]
			if _, err := svc.Import(ctx, admin, "acme", swapped, licenses.Create); !errors.Is(err, licenses.ErrRefused) || !errors.Is(err, license.ErrSignature) {
				t.Fatalf("a broken signature: %v", err)
			}
			v, err := svc.Import(ctx, admin, "acme", ok, licenses.Create)
			if err != nil || v.Name != "tresor" || v.Status != license.Valid || v.Source != store.LicenseImport {
				t.Fatalf("an import: %+v %v", v, err)
			}
			if _, err := svc.Import(ctx, admin, "acme", lic.sign("tresor", "max", now, now+day), v.Version+1); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("a stale If-Match: %v", err)
			}
			if v, err = svc.Import(ctx, admin, "acme", lic.sign("tresor", "max", now, now+day), v.Version); err != nil || v.Plan != "max" {
				t.Fatalf("a replacement: %+v %v", v, err)
			}
			// past its grace: served no more, and removed by the expiry pass
			l, _ := en.st.GetLicense(ctx, en.tenantID(t), "tresor")
			if err := en.st.InTxLocks(ctx, nil, func(tx *store.Tx) error {
				l.GraceUntil = now - 1
				return tx.PutLicense(ctx, &l, l.Version)
			}); err != nil {
				t.Fatal(err)
			}
			if _, served, _ := svc.Serve(ctx, en.tenantID(t), "tresor"); served {
				t.Fatal("served past its grace")
			}
			if err := svc.ExpireOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := en.st.GetLicense(ctx, en.tenantID(t), "tresor"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("after the expiry pass: %v", err)
			}
		})
	}
}

func (en *env) tenantID(t *testing.T) string {
	t.Helper()
	tn, err := en.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	return tn.ID
}
