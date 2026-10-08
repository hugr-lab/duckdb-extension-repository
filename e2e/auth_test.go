package e2e

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0006 phase 2: a private release over https with a Bearer token from an http secret.

const e2eIssuer = "https://idp.e2e.example"

type e2eIDP struct{ key *rsa.PrivateKey }

func (f *e2eIDP) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	switch url {
	case e2eIssuer + "/.well-known/openid-configuration":
		return []byte(`{"issuer":"` + e2eIssuer + `","jwks_uri":"` + e2eIssuer + `/jwks"}`), http.Header{}, nil
	case e2eIssuer + "/jwks":
		b, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes())}}})
		return b, http.Header{}, nil
	}
	return nil, nil, errors.New("not found")
}

func (f *e2eIDP) token(t *testing.T, sub, aud string) string {
	t.Helper()
	now := time.Now()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." +
		enc(map[string]any{"iss": e2eIssuer, "sub": sub, "aud": aud, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	d := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestServePrivate(t *testing.T) {
	b := needBuild(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: key}
	k := startKistaWith(t, b, &auth.Verifier{Fetch: idp})
	ctx := context.Background()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + k.addr}
	if _, err := adm.AddIssuer(ctx, serveAdmin, "acme", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	if err := adm.AddAudience(ctx, serveAdmin, "acme", "api://kista-acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|alice", []string{"install"}, "prod", ""); err != nil {
		t.Fatal(err)
	}
	k.release("httpfs", release.AddOptions{})
	k.release("loadable_extension_demo", release.AddOptions{Private: true})
	pemKey := k.pem(t, k.activeKey(t))

	install := func(token string) result {
		stmts := []string{
			createRepo("boot", k.httpURL(), pemKey),
			"INSTALL httpfs FROM boot",
			"LOAD httpfs FROM boot",
			"SET ca_cert_file = " + sqlString(testCA(t).caFile),
			createRepo("r", k.httpsURL(), pemKey),
		}
		if token != "" {
			stmts = append(stmts, "CREATE SECRET s (TYPE http, BEARER_TOKEN "+sqlString(token)+", SCOPE "+sqlString(k.httpsURL()+"/")+")")
		}
		stmts = append(stmts, "INSTALL loadable_extension_demo FROM r")
		res := newSession(t, b, nil).exec(stmts...)
		mustOK(t, res[:len(res)-1])
		return res[len(res)-1]
	}
	// a second tenant with the same IdP and its own audience
	ten := &tenants.Service{Store: k.st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
	if _, err := ten.CreateTenant(ctx, serveAdmin, "beta", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddIssuer(ctx, serveAdmin, "beta", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	if err := adm.AddAudience(ctx, serveAdmin, "beta", "api://kista-beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "beta", "subject:corp|alice", []string{"install"}, "", ""); err != nil {
		t.Fatal(err)
	}
	canonical := "https://" + k.addr + "/acme"
	for _, aud := range []string{"api://kista-acme", canonical} {
		if r := install(idp.token(t, "alice", aud)); !r.OK {
			t.Fatalf("granted token for %s: %s", aud, r.Error)
		}
	}
	file := "/loadable_extension_demo.duckdb_extension.gz"
	for name, c := range map[string]struct {
		tok  string
		want int
	}{
		"no token":           {"", 401},
		"no grant":           {idp.token(t, "bob", "api://kista-acme"), 404},
		"another tenant's":   {idp.token(t, "alice", "api://kista-beta"), 401},
		"another canonical":  {idp.token(t, "alice", "https://"+k.addr+"/beta"), 401},
		"not a token at all": {"garbage", 401},
	} {
		mark := k.mark()
		r := install(c.tok)
		if r.OK {
			t.Fatalf("%s: installed a private extension", name)
		}
		got := k.statuses(file, mark)
		if len(got) == 0 || got[0] != c.want {
			t.Fatalf("%s: statuses %v, want %d", name, got, c.want)
		}
	}
}

// The node agent's question (spec 0007): with the token its DuckDB secret uses, ask whether
// name@version is installable for the pinned DuckDB and platform, install it, then see a yank.
func TestAPIItemThenInstall(t *testing.T) {
	b := needBuild(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: key}
	k := startKistaWith(t, b, &auth.Verifier{Fetch: idp})
	ctx := context.Background()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + k.addr}
	if _, err := adm.AddIssuer(ctx, serveAdmin, "acme", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|alice", []string{"install"}, "", ""); err != nil {
		t.Fatal(err)
	}
	k.release("httpfs", release.AddOptions{})
	rel := k.release("loadable_extension_demo", release.AddOptions{Private: true})
	k.release("demo_capi", release.AddOptions{Private: true})
	tok := idp.token(t, "alice", "https://"+k.addr+"/acme")

	pool := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(testCA(t).caFile)
	pool.AppendCertsFromPEM(pemBytes)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	get := func(path string) map[string]any {
		req, _ := http.NewRequest("GET", "https://"+k.addr+"/api/v1/tenants/acme/channels/prod/extensions/"+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, err)
		}
		return m
	}
	ask := func() map[string]any {
		return get("loadable_extension_demo/versions/default-version?duckdb_version=" + b.versionDir + "&platform=" + b.platform)
	}
	// the C API build is current on this DuckDB, by the path DuckDB uses
	rows := get("demo_capi?duckdb_version=" + b.versionDir + "&platform=" + b.platform)["releases"].([]any)
	if len(rows) != 1 {
		t.Fatalf("demo_capi rows: %v", rows)
	}
	if cf := rows[0].(map[string]any)["current_for"].([]any); len(cf) != 1 || cf[0] != b.versionDir {
		t.Fatalf("demo_capi current_for: %v", rows[0])
	}
	if m := ask(); m["status"] != "available" || m["current"] != true {
		t.Fatalf("before install: %v", m)
	}
	pemKey := k.pem(t, k.activeKey(t))
	res := newSession(t, b, nil).exec(
		createRepo("boot", k.httpURL(), pemKey),
		"INSTALL httpfs FROM boot",
		"LOAD httpfs FROM boot",
		"SET ca_cert_file = "+sqlString(testCA(t).caFile),
		createRepo("r", k.httpsURL(), pemKey),
		"CREATE SECRET s (TYPE http, BEARER_TOKEN "+sqlString(tok)+", SCOPE "+sqlString(k.httpsURL()+"/")+")",
		"INSTALL loadable_extension_demo FROM r VERSION 'default-version'",
		"LOAD loadable_extension_demo FROM r",
	)
	mustOK(t, res)
	if _, err := k.rel.Apply(ctx, serveAdmin, "acme", "prod", rel.ID, release.Yank); err != nil {
		t.Fatal(err)
	}
	if m := ask(); m["status"] != "yanked" {
		t.Fatalf("after the yank: %v", m)
	}
	// and DuckDB cannot install it any more; the C API build still installs by the flat path
	res = newSession(t, b, nil).exec(
		createRepo("boot", k.httpURL(), pemKey),
		"INSTALL httpfs FROM boot",
		"LOAD httpfs FROM boot",
		"SET ca_cert_file = "+sqlString(testCA(t).caFile),
		createRepo("r", k.httpsURL(), pemKey),
		"CREATE SECRET s (TYPE http, BEARER_TOKEN "+sqlString(tok)+", SCOPE "+sqlString(k.httpsURL()+"/")+")",
		"FORCE INSTALL loadable_extension_demo FROM r VERSION 'default-version'",
		"INSTALL demo_capi FROM r",
	)
	mustOK(t, res[:6])
	mustFail(t, res[6], "failed to install")
	mustOK(t, res[7:])
}
