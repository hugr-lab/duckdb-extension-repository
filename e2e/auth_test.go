package e2e

import (
	"bytes"
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
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0006 phase 2: a private release over https with a Bearer token from an http secret.

const (
	e2eIssuer = "https://idp.e2e.example"
	e2eGitHub = "https://token.actions.e2e.example" // a trusted-publishing provider (spec 0008)
)

type e2eIDP struct{ key *rsa.PrivateKey }

func (f *e2eIDP) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	for _, iss := range []string{e2eIssuer, e2eGitHub} {
		if url == iss+"/.well-known/openid-configuration" {
			return []byte(`{"issuer":"` + iss + `","jwks_uri":"` + iss + `/jwks"}`), http.Header{}, nil
		}
	}
	switch url {
	case e2eIssuer + "/jwks", e2eGitHub + "/jwks":
		b, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes())}}})
		return b, http.Header{}, nil
	}
	return nil, nil, errors.New("not found")
}

func (f *e2eIDP) token(t *testing.T, sub, aud string) string {
	t.Helper()
	return f.sign(t, map[string]any{"iss": e2eIssuer, "sub": sub, "aud": aud})
}

// run is a GitHub Actions OIDC token of hugr-lab/duckdb-acl's release workflow.
func (f *e2eIDP) run(t *testing.T, aud string) string {
	t.Helper()
	return f.sign(t, map[string]any{"iss": e2eGitHub, "sub": "repo:hugr-lab/duckdb-acl:ref:refs/tags/v1.0", "aud": aud,
		"repository_owner_id": "10", "repository_id": "20", "repository": "hugr-lab/duckdb-acl", "ref": "refs/tags/v1.0",
		"workflow_ref": "hugr-lab/duckdb-acl/.github/workflows/release.yml@refs/tags/v1.0", "event_name": "push", "sha": "abc"})
}

func (f *e2eIDP) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	now := time.Now()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	claims["iat"], claims["exp"] = now.Unix(), now.Add(30*time.Minute).Unix()
	input := enc(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + enc(claims)
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
	if _, err := k.rel.Apply(ctx, serveAdmin, "acme", "prod", "", rel.ID, release.Yank, 0); err != nil {
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

// Spec 0008: CI publishes into staging through the API, a release manager promotes the version to
// prod, DuckDB installs and loads it; reserved names need grants naming them; a binary under
// another name is refused; a block stops installs.
func TestPublishPromoteInstall(t *testing.T) {
	b := needBuild(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: key}
	k := startKistaWith(t, b, &auth.Verifier{Fetch: idp})
	ctx := context.Background()
	aud := "https://" + k.addr + "/acme"
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: aud[:len(aud)-5],
		Providers: auth.Providers{{Name: "github", URL: e2eGitHub}}}
	if _, err := adm.AddIssuer(ctx, serveAdmin, "acme", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	// a staging channel with its own key
	if _, err := k.ten.CreateChannel(ctx, serveAdmin, "acme", "staging", store.ChannelSigned); err != nil {
		t.Fatal(err)
	}
	if _, err := k.ten.SetChannelVersions(ctx, serveAdmin, "acme", "staging", []string{b.versionDir}, nil); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(k.keyDir, "s.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := k.keys.Add(ctx, serveAdmin, "acme", "staging", "file:s.pem", true); err != nil {
		t.Fatal(err)
	}
	// CI is a publisher bound to the release workflow (trusted publishing)
	if _, err := adm.AddPublisher(ctx, serveAdmin, "acme", "ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGitHubCredential(ctx, serveAdmin, "acme", "ci", store.GitHubCredential{OwnerID: "10", RepositoryID: "20",
		Workflow: "hugr-lab/duckdb-acl/.github/workflows/release.yml", Ref: "refs/tags/v*"}); err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct{ principal, verb, channel, ext string }{
		{"publisher:ci", "publish", "staging", ""},          // channel-wide: not demo_capi, a core name
		{"publisher:ci", "publish", "staging", "demo_capi"}, // named
	} {
		if _, err := adm.AddGrant(ctx, serveAdmin, "acme", g.principal, []string{g.verb}, g.channel, g.ext); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []struct{ sub, verb, channel, ext string }{
		{"cw", "publish", "staging", ""}, // channel-wide only
		{"rm", "promote", "prod", "loadable_extension_demo"},
		{"rm", "publish", "staging", "loadable_extension_demo"},
		{"tom", "admin", "", ""},
	} {
		if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|"+g.sub, []string{g.verb}, g.channel, g.ext); err != nil {
			t.Fatal(err)
		}
	}
	pool := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(testCA(t).caFile)
	pool.AppendCertsFromPEM(pemBytes)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	call := func(method, path, sub, ctype string, body []byte) (int, map[string]any) {
		req, _ := http.NewRequest(method, "https://"+k.addr+path, bytes.NewReader(body))
		tok := idp.token(t, sub, aud)
		if sub == "ci" {
			tok = idp.run(t, aud)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	file := func(name string) []byte {
		data, err := os.ReadFile(b.extensions[name])
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	q := "/releases?version=default-version&platform=" + b.platform + "&visibility=public"
	S := "/api/v1/tenants/acme/channels/staging/extensions/"
	if st, m := call("POST", S+"loadable_extension_demo"+q, "ci", "application/octet-stream", file("loadable_extension_demo")); st != 201 {
		t.Fatalf("publish: %d %v", st, m)
	}
	// the demo's binary under another name: its entry point is not that name's
	if st, m := call("POST", S+"other_ext"+q, "ci", "application/octet-stream", file("loadable_extension_demo")); st != 400 ||
		!strings.Contains(fmt.Sprint(m["detail"]), "entry point") {
		t.Fatalf("another name: %d %v", st, m)
	}
	// demo_capi is a core name: the channel-wide grant does not reach it, the named one does
	capi := file("demo_capi")
	f, err := extfile.Open(bytes.NewReader(capi), int64(len(capi)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	capiPath := S + "demo_capi/releases?version=" + f.Metadata.ExtensionVersion + "&platform=" + b.platform + "&visibility=public"
	if st, m := call("POST", capiPath, "cw", "application/octet-stream", capi); st != 404 {
		t.Fatalf("demo_capi with a channel-wide grant only: %d %v", st, m)
	}
	if st, m := call("POST", capiPath, "ci", "application/octet-stream", capi); st != 201 || m["shadows"] != "core" {
		t.Fatalf("demo_capi with a named grant: %d %v", st, m)
	}
	// promote the version to prod
	st, m := call("POST", "/api/v1/tenants/acme/channels/prod/extensions/loadable_extension_demo/releases/promote", "rm",
		"application/json", []byte(`{"from_channel":"staging","version":"default-version"}`))
	if st != 201 {
		t.Fatalf("promote: %d %v", st, m)
	}
	hash := m["releases"].([]any)[0].(map[string]any)["body_hash"].(string)
	pemKey := k.pem(t, k.activeKey(t))
	res := newSession(t, b, nil).exec(
		createRepo("p", k.httpURL(), pemKey),
		"INSTALL loadable_extension_demo FROM p",
		"LOAD loadable_extension_demo FROM p",
	)
	mustOK(t, res)
	// a block yanks it: DuckDB cannot install it any more
	if st, m := call("POST", "/api/v1/tenants/acme/blocks", "tom", "application/json",
		[]byte(`{"body_hash":"`+hash+`","reason":"e2e"}`)); st != 201 {
		t.Fatalf("block: %d %v", st, m)
	}
	res = newSession(t, b, nil).exec(
		createRepo("p", k.httpURL(), pemKey),
		"FORCE INSTALL loadable_extension_demo FROM p",
	)
	mustFail(t, res[1], "failed to download") // 401: as missing, to an anonymous client
}
