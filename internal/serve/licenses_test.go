package serve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// licenseEnv is an env with licensing on, and sign, which signs a licence for a name with the
// vectors' test key after recording that key on the name's builds (spec 0021).
func licenseEnv(t *testing.T, scheme string) (*env, *fakeIDP, *tenants.AuthAdmin, *licenses.Service, func(ext string) string) {
	t.Helper()
	en, idp, adm := authEnv(t, scheme)
	k, err := vectors.LoadKey("../license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}
	svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Log: slog.New(slog.DiscardHandler)}
	en.h.o.Licenses = licenses.Served{Service: svc}
	n := 0
	sign := func(ext string) string {
		n++
		tn, err := en.st.GetTenant(ctx, "acme")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storetest.LicenseKey(ctx, en.st, tn.ID, ext, store.KeySourcePublisher, "ci", &k.PublicKey); err != nil {
			t.Fatal(err)
		}
		now := time.Now().Unix()
		return vectors.Sign(k, vectors.Header(k), fmt.Sprintf(
			`{"v":1,"iss":"https://issuer.example","jti":"lic-%d","sub":"acme","ext":%q,"plan":"pro","iat":%d,"exp":%d}`, n, ext, now-60, now+86400))
	}
	return en, idp, adm, svc, sign
}

// Spec 0012: the licence routes decide as the binary routes do, and answer a caller who may not
// have a licence exactly as a missing path.
func TestLicenseRoutes(t *testing.T) {
	en, idp, adm, svc, sign := licenseEnv(t, "https")
	tresor := en.add(t, ext(t, 2000, 1, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "tresor"})
	secret := en.add(t, ext(t, 2000, 2, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "secret", Private: true})
	en.add(t, ext(t, 2000, 3, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "nolic"})
	tresorTok, secretTok := sign("tresor"), sign("secret")
	for _, tok := range []string{tresorTok, secretTok} {
		if _, err := svc.Import(ctx, admin, "acme", tok, licenses.Create); err != nil {
			t.Fatal(err)
		}
	}
	tok := idp.token(t, nil)
	get := func(path string, hdr map[string]string) answer { return en.do(t, "GET", path, hdr) }
	const (
		lic     = "/acme/prod/-/licenses/"
		binMiss = "/acme/prod/tresor/9.9/v2.0.0/linux_amd64/tresor.duckdb_extension.gz"
	)
	same := func(name string, a, b answer) {
		t.Helper()
		if a.status != b.status || !bytes.Equal(a.body, b.body) || !sameHeaders(a.header, b.header) {
			t.Fatalf("%s: %d %q %v, want %d %q %v", name, a.status, a.body, a.header, b.status, b.body, b.header)
		}
	}

	// no token: a private licence answers as a missing path (401), as does a missing licence
	same("anonymous, private licence", get(lic+"tresor", nil), get(binMiss, nil))
	same("anonymous, no licence", get(lic+"nolic", nil), get(binMiss, nil))
	same("anonymous, the list", get("/acme/prod/-/licenses", nil), get(binMiss, nil))
	// a token without install: 404, as a missing path for it
	same("no grant", get(lic+"tresor", bearerHdr(tok)), get(binMiss, bearerHdr(tok)))

	// install on tresor: its licence, never cached, with an ETag
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{Extension: "tresor"}); err != nil {
		t.Fatal(err)
	}
	a := get(lic+"tresor", bearerHdr(tok))
	if a.status != 200 || string(a.body) != tresorTok || a.header.Get("Content-Type") != "application/jwt" ||
		a.header.Get("Cache-Control") != "private, no-store" || a.header.Get("Vary") != "Authorization" || a.header.Get("ETag") == "" {
		t.Fatalf("granted: %d %q %v", a.status, a.body, a.header)
	}
	h := bearerHdr(tok)
	h["If-None-Match"] = a.header.Get("ETag")
	if c := get(lic+"tresor", h); c.status != 304 || len(c.body) != 0 {
		t.Fatalf("an unchanged licence: %d", c.status)
	}
	if c := en.do(t, "HEAD", lic+"tresor", bearerHdr(tok)); c.status != 200 || len(c.body) != 0 {
		t.Fatalf("HEAD: %d %d", c.status, len(c.body))
	}
	same("granted, another name", get(lic+"secret", bearerHdr(tok)), get(binMiss, bearerHdr(tok)))
	same("granted, no licence", get(lic+"nolic", bearerHdr(tok)), get(binMiss, bearerHdr(tok)))
	if c := en.do(t, "POST", lic+"tresor", bearerHdr(tok)); c.status != 405 {
		t.Fatalf("POST: %d", c.status)
	}

	// the list: the names the caller may use
	var out struct{ Licenses []string }
	if err := json.Unmarshal(get("/acme/prod/-/licenses", bearerHdr(tok)).body, &out); err != nil || !slices.Equal(out.Licenses, []string{tresorTok}) {
		t.Fatalf("the list: %v %v", out, err)
	}

	// a public licence: to whoever is served a public release of the name, and only then
	for _, name := range []string{"tresor", "secret"} {
		if _, err := svc.SetVisibility(ctx, admin, "acme", name, store.Public, 0); err != nil {
			t.Fatal(err)
		}
	}
	if c := get(lic+"tresor", nil); c.status != 200 || string(c.body) != tresorTok {
		t.Fatalf("a public licence, anonymous: %d", c.status)
	}
	same("a public licence of a private release, anonymous", get(lic+"secret", nil), get(binMiss, nil))

	// no live release, no licence: a yanked release takes it with it
	if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", secret.ID, release.Yank, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{Extension: "secret"}); err != nil {
		t.Fatal(err)
	}
	same("a yanked release's licence", get(lic+"secret", bearerHdr(tok)), get(binMiss, bearerHdr(tok)))

	// a passthrough channel has none; a refused key, or licensing off, serves none
	if c := get("/acme/mirror/-/licenses/tresor", nil); c.status != 404 {
		t.Fatalf("passthrough: %d", c.status)
	}
	v, err := svc.Get(ctx, admin, "acme", "tresor")
	if err != nil {
		t.Fatal(err)
	}
	svc.Refuse = map[string]bool{v.Key: true}
	same("a refused key", get(lic+"tresor", bearerHdr(tok)), get(binMiss, bearerHdr(tok)))
	svc.Refuse = nil
	svc.Enabled = false
	same("licensing off", get(lic+"tresor", bearerHdr(tok)), get(binMiss, bearerHdr(tok)))
	svc.Enabled = true

	// spec 0021: served only where a live build declares its key: tresor 1.0 is yanked, and 1.1
	// declares none
	if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", tresor.ID, release.Yank, 0); err != nil {
		t.Fatal(err)
	}
	en.add(t, ext(t, 2000, 4, cpp("1.1")), release.AddOptions{Unchecked: true, Name: "tresor"})
	same("no live build declares the key", get(lic+"tresor", bearerHdr(tok)), get(binMiss, bearerHdr(tok)))
	if _, err := svc.Get(ctx, admin, "acme", "tresor"); err != nil {
		t.Fatalf("the licence is no longer stored: %v", err)
	}

	// paths under "-" that are not licence routes
	for _, p := range []string{"/acme/prod/-", "/acme/prod/-/x", "/acme/prod/-/licenses/Bad", "/acme/prod/-/licenses/a/b"} {
		if c := get(p, nil); c.status != 404 {
			t.Errorf("%s: %d", p, c.status)
		}
	}
}

// Over plain http a token is ignored: only a public licence of a public release is served.
func TestLicenseOverHTTP(t *testing.T) {
	en, idp, adm, svc, sign := licenseEnv(t, "http")
	en.add(t, ext(t, 2000, 1, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "tresor"})
	if _, err := svc.Import(ctx, admin, "acme", sign("tresor"), licenses.Create); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{Extension: "tresor"}); err != nil {
		t.Fatal(err)
	}
	if c := en.do(t, "GET", "/acme/prod/-/licenses/tresor", bearerHdr(idp.token(t, nil))); c.status != 401 {
		t.Fatalf("a private licence over http with a token: %d", c.status)
	}
	if _, err := svc.SetVisibility(ctx, admin, "acme", "tresor", store.Public, 0); err != nil {
		t.Fatal(err)
	}
	if c := en.do(t, "GET", "/acme/prod/-/licenses/tresor", nil); c.status != 200 {
		t.Fatalf("a public licence over http: %d", c.status)
	}
}
