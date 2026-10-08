package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

var (
	ctx   = context.Background()
	admin = authz.Actor{Kind: authz.ActorOS, ID: "1:test"}
)

const (
	idpURL    = "https://idp.example"
	publicURL = "https://kista.example"
)

type fakeIDP struct{ key *rsa.PrivateKey }

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIDP) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	switch url {
	case idpURL + "/.well-known/openid-configuration":
		return []byte(`{"issuer":"` + idpURL + `","jwks_uri":"` + idpURL + `/jwks"}`), http.Header{}, nil
	case idpURL + "/jwks":
		b, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
			"n": b64u(f.key.N.Bytes()), "e": b64u(big.NewInt(int64(f.key.E)).Bytes())}}})
		return b, http.Header{}, nil
	}
	return nil, nil, errors.New("not found")
}

func (f *fakeIDP) token(t *testing.T, sub string) string {
	t.Helper()
	now := time.Now()
	enc := func(v any) string { b, _ := json.Marshal(v); return b64u(b) }
	input := enc(map[string]any{"alg": "RS256", "kid": "k1"}) + "." + enc(map[string]any{"iss": idpURL, "sub": sub,
		"aud": publicURL + "/acme", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	d := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, d[:])
	return input + "." + b64u(sig)
}

func ext(t *testing.T, seed uint64, m extfile.Metadata) []byte {
	t.Helper()
	r := mrand.New(mrand.NewPCG(seed, seed))
	b := make([]byte, 3000)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	block, err := extfile.EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	b = append(append(b, extfile.MetadataPrefix...), block[:]...)
	return append(b, make([]byte, extfile.SignatureSize)...)
}

func cpp(v string) extfile.Metadata {
	return extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: v, ABI: extfile.ABICPP}
}

func capi(v, c string) extfile.Metadata {
	return extfile.Metadata{Platform: "linux_amd64", CAPIVersion: c, ExtensionVersion: v, ABI: extfile.ABICStruct}
}

type env struct {
	st    *store.Store
	rel   *release.Service
	ten   *tenants.Service
	adm   *tenants.AuthAdmin
	idp   *fakeIDP
	api   *api.Handler
	serve *serve.Handler
	ids   map[string]string // label -> release id
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ks := &keys.Service{Store: st, Signers: signer.Resolver{FileDir: dir, AllowFile: true}, Authz: authz.ServerAdmin{}}
	fst, _ := fs.Open(filepath.Join(t.TempDir(), "blobs"))
	bs, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: fst}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 1 << 20, MaxIngests: 4, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bs.Close() })
	ten := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ten.CreateTenant(ctx, admin, "acme", "", ""))
	must(ten.AddVersion(ctx, admin, "v2.0.0", "release", []string{"v1.5.6", "v2.0.0"}))
	must(ten.AddVersion(ctx, admin, "v1.9.0", "release", []string{"v1.4.0"}))
	for _, ch := range []string{"prod", "staging"} {
		must(ten.CreateChannel(ctx, admin, "acme", ch, store.ChannelSigned))
		must(ten.SetChannelVersions(ctx, admin, "acme", ch, []string{"v2.0.0", "v1.9.0"}, nil))
		if err := signer.GenerateKeyFile(filepath.Join(dir, ch+".pem")); err != nil {
			t.Fatal(err)
		}
		must(ks.Add(ctx, admin, "acme", ch, "file:"+ch+".pem", true))
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	en := &env{st: st, ten: ten, idp: &fakeIDP{key: key}, ids: map[string]string{},
		rel: &release.Service{Store: st, Blob: bs, Signers: ks, Authz: authz.ServerAdmin{}}}
	en.adm = &tenants.AuthAdmin{Store: st, Authz: authz.ServerAdmin{}, Fetch: en.idp, PublicURL: publicURL}
	must(en.adm.AddIssuer(ctx, admin, "acme", store.Issuer{Name: "corp", URL: idpURL}))
	auths, snaps := &auth.TenantAuths{Store: st}, &release.Snapshots{Store: st}
	verifier := &auth.Verifier{Fetch: en.idp}
	en.api = api.New(api.Options{Store: st, Snapshots: snaps, Auths: auths, Verifier: verifier, PublicURL: publicURL,
		Rate: 1000, Burst: 1000, Log: slog.New(slog.DiscardHandler), KistaVersion: "test"})
	en.serve = serve.NewHandler(st, ks, bs, serve.Options{MaxDownloads: 8, MaxDownloadsPerClient: 8, MinRate: 1024,
		WriteIdleTimeout: 10 * time.Second, Log: slog.New(slog.DiscardHandler), Verifier: verifier, PublicURL: publicURL,
		Auths: auths, Snapshots: snaps, API: en.api})
	en.serve.SetReady(true)

	add := func(label, channel, name string, m extfile.Metadata, seed uint64, private bool, notCurrent ...bool) {
		r, _, err := en.rel.Add(ctx, admin, "acme", channel, bytes.NewReader(ext(t, seed, m)),
			release.AddOptions{Name: name, Private: private, NotCurrent: len(notCurrent) > 0})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		en.ids[label] = r.ID
	}
	add("tresor-0.9", "prod", "tresor", cpp("0.9"), 1, false)
	add("tresor-1.0", "prod", "tresor", cpp("1.0"), 2, false)
	add("tresor-1.1", "prod", "tresor", cpp("1.1"), 3, true)
	add("demo-v1", "prod", "demo", capi("0.1", "v1.2.0"), 4, false)
	add("demo-v2", "prod", "demo", capi("0.1", "v2.0.0"), 5, false)
	add("acl", "prod", "acl", cpp("1.0"), 6, true)
	add("acl-staging", "staging", "acl", cpp("1.0"), 7, true)
	add("tresor-1.2", "prod", "tresor", cpp("1.2"), 8, false, true) // published, not current
	if _, err := en.rel.Apply(ctx, admin, "acme", "prod", en.ids["tresor-0.9"], release.Yank); err != nil {
		t.Fatal(err)
	}
	return en
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

// get calls the API through kista serve's handler, as https.
func (en *env) get(t *testing.T, path, token string, hdr ...string) resp {
	t.Helper()
	return en.do(t, "GET", path, token, hdr...)
}

func (en *env) do(t *testing.T, method, path, token string, hdr ...string) resp {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.TLS = nil
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	en.serve.ServeHTTP(rec, serve.WithHTTPS(req))
	return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.status, r.body)
	}
	return m
}

func names(m map[string]any, key, field string) []string {
	var out []string
	for _, x := range m[key].([]any) {
		out = append(out, x.(map[string]any)[field].(string))
	}
	return out
}

func TestIndexViews(t *testing.T) {
	en := newEnv(t)
	const base = "/api/v1/tenants/acme/channels/prod"

	if r := en.get(t, "/api/v1/info", ""); r.status != 200 || r.json(t)["api"] != "1" {
		t.Fatalf("info: %d %s", r.status, r.body)
	}
	if r := en.get(t, "/api/v1/tenants/acme/channels", ""); r.status != 200 || len(names(r.json(t), "channels", "name")) != 2 {
		t.Fatalf("channels: %d %s", r.status, r.body)
	}
	ci := en.get(t, base, "").json(t)
	if len(ci["duckdb_versions"].([]any)) != 2 || len(ci["keys"].([]any)) != 1 {
		t.Fatalf("channel: %v", ci)
	}

	// anonymous: public names only; a private-only name is invisible
	anon := en.get(t, base+"/extensions", "")
	if got := names(anon.json(t), "extensions", "name"); len(got) != 2 || got[0] != "demo" || got[1] != "tresor" {
		t.Fatalf("anonymous names: %v", got)
	}
	if anon.header.Get("Cache-Control") != "public, no-cache" {
		t.Fatalf("anonymous cache-control: %v", anon.header)
	}
	// current per caller: anonymous gets 1.0 (1.1 is private)
	cur := en.get(t, base+"/extensions?duckdb_version=v2.0.0&platform=linux_amd64", "").json(t)
	for _, e := range cur["extensions"].([]any) {
		m := e.(map[string]any)
		if m["name"] == "tresor" && m["current"] != "1.0" {
			t.Fatalf("anonymous current: %v", m)
		}
	}
	// a token without grants sees the same; whoami works
	tok := en.idp.token(t, "alice")
	if got := names(en.get(t, base+"/extensions", tok).json(t), "extensions", "name"); len(got) != 2 {
		t.Fatalf("token without grants: %v", got)
	}
	if w := en.get(t, "/api/v1/tenants/acme/whoami", tok).json(t); w["principals"].([]any)[0] != "issuer:corp" {
		t.Fatalf("whoami: %v", w)
	}
	// install on acl in prod only: acl appears in prod, not in staging
	if _, err := en.adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, "prod", "acl"); err != nil {
		t.Fatal(err)
	}
	if got := names(en.get(t, base+"/extensions", tok).json(t), "extensions", "name"); len(got) != 3 || got[0] != "acl" {
		t.Fatalf("with install on acl: %v", got)
	}
	if got := names(en.get(t, "/api/v1/tenants/acme/channels/staging/extensions", tok).json(t), "extensions", "name"); len(got) != 0 {
		t.Fatalf("a grant in prod shows staging: %v", got)
	}
	// tresor's private 1.1 stays invisible to this caller (the grant is on acl)
	rows := en.get(t, base+"/extensions/tresor", tok).json(t)["releases"].([]any)
	for _, r := range rows {
		if r.(map[string]any)["version"] == "1.1" {
			t.Fatal("a private release outside the grant is listed")
		}
	}

	// an invalid token is 401 on the API
	bad := en.get(t, base+"/extensions", "garbage")
	if bad.status != 401 || bad.header.Get("WWW-Authenticate") == "" {
		t.Fatalf("invalid token: %d", bad.status)
	}
	// unknown tenant, channel and routes: 404 with constant bytes
	a, b := en.get(t, "/api/v1/tenants/nope/channels", ""), en.get(t, base+"/nothing", "")
	if a.status != 404 || b.status != 404 || !bytes.Equal(a.body, b.body) {
		t.Fatalf("404s: %d %d", a.status, b.status)
	}
	// 304 on the ETag
	if r := en.get(t, base+"/extensions", "", "If-None-Match", anon.header.Get("ETag")); r.status != 304 {
		t.Fatalf("304: %d", r.status)
	}
	// pagination by name
	p1 := en.get(t, base+"/extensions?limit=1", "").json(t)
	if names(p1, "extensions", "name")[0] != "demo" || p1["next"] == nil {
		t.Fatalf("page 1: %v", p1)
	}
	p2 := en.get(t, base+"/extensions?limit=1&cursor="+p1["next"].(string), "").json(t)
	if names(p2, "extensions", "name")[0] != "tresor" || p2["next"] != nil {
		t.Fatalf("page 2: %v", p2)
	}
}

func TestItemLookup(t *testing.T) {
	en := newEnv(t)
	const base = "/api/v1/tenants/acme/channels/prod/extensions/"
	status := func(name, v string) string {
		return en.get(t, base+name+"/versions/"+v+"?duckdb_version=v2.0.0&platform=linux_amd64", "").json(t)["status"].(string)
	}
	for name, want := range map[string]string{"tresor/1.0": "available", "tresor/0.9": "yanked", "tresor/9.9": "missing",
		"tresor/1.1": "missing", "acl/1.0": "missing", "demo/0.1": "available"} {
		n, v, _ := cut(name)
		if got := status(n, v); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	if _, err := en.rel.Apply(ctx, admin, "acme", "prod", en.ids["tresor-1.0"], release.Deprecate); err != nil {
		t.Fatal(err)
	}
	if got := status("tresor", "1.0"); got != "deprecated" {
		t.Fatalf("after deprecate: %s", got)
	}
	// demo 0.1 on v2.0.0 is the C API v2 build (the highest major the version accepts)
	m := en.get(t, base+"demo/versions/0.1?duckdb_version=v2.0.0&platform=linux_amd64", "").json(t)
	if m["release"].(map[string]any)["build_c_api"] != "v2.0.0" {
		t.Fatalf("demo on v2.0.0: %v", m)
	}
	if r := en.get(t, base+"demo/versions/0.1", ""); r.status != 400 {
		t.Fatalf("without duckdb_version: %d", r.status)
	}
	// published but not current
	if m := en.get(t, base+"tresor/versions/1.2?duckdb_version=v2.0.0&platform=linux_amd64", "").json(t); m["status"] != "available" ||
		m["current"] != false {
		t.Fatalf("tresor 1.2: %v", m)
	}

	// yanking the C API v2 build: the path serves the v1 build, and the yanked one comes along, so a
	// node that installed it learns it; the v2 row serves nothing and would serve v2.0.0
	v2hash := m["release"].(map[string]any)["body_hash"]
	if _, err := en.rel.Apply(ctx, admin, "acme", "prod", en.ids["demo-v2"], release.Yank); err != nil {
		t.Fatal(err)
	}
	m = en.get(t, base+"demo/versions/0.1?duckdb_version=v2.0.0&platform=linux_amd64", "").json(t)
	y, _ := m["yanked"].([]any)
	if m["status"] != "available" || m["release"].(map[string]any)["build_c_api"] != "v1.2.0" || len(y) != 1 ||
		y[0].(map[string]any)["body_hash"] != v2hash {
		t.Fatalf("demo after yanking v2: %v", m)
	}
	rows := en.get(t, base+"demo?duckdb_version=v2.0.0&platform=linux_amd64", "").json(t)["releases"].([]any)
	for _, x := range rows {
		r := x.(map[string]any)
		if r["state"] == store.ReleaseYanked && (len(r["serves"].([]any)) != 0 || r["would_serve"].([]any)[0] != "v2.0.0") {
			t.Fatalf("yanked row: %v", r)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("demo rows on v2.0.0: %v", rows)
	}

	// a private yanked release: missing to anonymous, yanked to an install holder
	if _, err := en.rel.Apply(ctx, admin, "acme", "prod", en.ids["acl"], release.Yank); err != nil {
		t.Fatal(err)
	}
	if got := status("acl", "1.0"); got != "missing" {
		t.Fatalf("private yanked, anonymous: %s", got)
	}
	if _, err := en.adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, "prod", "acl"); err != nil {
		t.Fatal(err)
	}
	tok := en.idp.token(t, "alice")
	if m := en.get(t, base+"acl/versions/1.0?duckdb_version=v2.0.0&platform=linux_amd64", tok).json(t); m["status"] != "yanked" {
		t.Fatalf("private yanked, install holder: %v", m)
	}
}

func cut(s string) (string, string, bool) {
	for i := range s {
		if s[i] == '/' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// The index agrees with what the DuckDB routes serve: for every (DuckDB version, platform, name,
// version), the versioned path answers 200 exactly when a row serves it, with that row's body; the
// flat path serves the row marked current.
func TestIndexMatchesServing(t *testing.T) {
	en := newEnv(t)
	if _, err := en.adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, "", ""); err != nil {
		t.Fatal(err)
	}
	// four views: anonymous, a grant on acl in prod, a channel-wide grant, a tenant-wide grant
	views := []struct {
		sub, channel, ext string
	}{{"", "", ""}, {"bob", "prod", "acl"}, {"carol", "prod", ""}, {"alice", "", ""}}
	// the C API v2 build of demo is yanked: the versioned path falls back to v1 on v2.0.0
	if _, err := en.rel.Apply(ctx, admin, "acme", "prod", en.ids["demo-v2"], release.Yank); err != nil {
		t.Fatal(err)
	}
	for _, vw := range views {
		token := ""
		if vw.sub != "" {
			token = en.idp.token(t, vw.sub)
			if vw.sub != "alice" {
				if _, err := en.adm.AddGrant(ctx, admin, "acme", "subject:corp|"+vw.sub, []string{"install"}, vw.channel, vw.ext); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, name := range []string{"tresor", "demo", "acl"} {
			rows := en.get(t, "/api/v1/tenants/acme/channels/prod/extensions/"+name, token).json(t)["releases"].([]any)
			served := map[string]string{} // versioned path -> body hash
			flat := map[string]string{}   // flat path -> body hash
			for _, x := range rows {
				r := x.(map[string]any)
				for _, s := range r["serves"].([]any) {
					sv := s.(map[string]any)
					if r["state"] == store.ReleaseYanked {
						t.Fatalf("a yanked row serves %v", sv)
					}
					if served[sv["path"].(string)] != "" {
						t.Fatalf("two rows serve %s", sv["path"])
					}
					served[sv["path"].(string)] = r["body_hash"].(string)
					if fp, ok := sv["flat_path"].(string); ok {
						flat[fp] = r["body_hash"].(string)
					}
				}
			}
			for _, v := range []string{"v2.0.0", "v1.9.0"} {
				for _, ver := range []string{"0.1", "0.9", "1.0", "1.1", "1.2"} {
					p := name + "/" + ver + "/" + v + "/linux_amd64/" + name + ".duckdb_extension.gz"
					checkPath(t, en, token, p, served[p])
				}
				fp := v + "/linux_amd64/" + name + ".duckdb_extension.gz"
				checkPath(t, en, token, fp, flat[fp])
			}
		}
	}
}

func checkPath(t *testing.T, en *env, token, path, wantHash string) {
	t.Helper()
	r := en.get(t, "/acme/prod/"+path, token)
	if wantHash == "" {
		if r.status == 200 {
			t.Fatalf("%s (token %v): served, but the index has no row for it", path, token != "")
		}
		return
	}
	if r.status != 200 {
		t.Fatalf("%s (token %v): %d, but the index lists it", path, token != "", r.status)
	}
	zr, err := gzip.NewReader(bytes.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	f, err := extfile.Open(bytes.NewReader(body), int64(len(body)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if f.Hash.String() != wantHash {
		t.Fatalf("%s: serves body %s, the index says %s", path, f.Hash, wantHash)
	}
}

func TestHTTPRules(t *testing.T) {
	en := newEnv(t)
	// the API is https only
	req := httptest.NewRequest("GET", "/api/v1/info", nil)
	rec := httptest.NewRecorder()
	en.serve.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("over http: %d", rec.Code)
	}
	// methods: 405 with Allow on a route, 404 on an unknown path
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if r := en.do(t, m, "/api/v1/info", ""); r.status != 405 || r.header.Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d %v", m, r.status, r.header)
		}
	}
	if r := en.do(t, "POST", "/api/v1/nothing", ""); r.status != 404 {
		t.Errorf("POST on an unknown path: %d", r.status)
	}
	// HEAD: headers without the body
	if r := en.do(t, "HEAD", "/api/v1/info", ""); r.status != 200 || len(r.body) != 0 || r.header.Get("ETag") == "" {
		t.Errorf("HEAD: %d %q", r.status, r.body)
	}
	// problems: a fixed type, constant 401 and 404 bytes
	nf := en.get(t, "/api/v1/tenants/nope/channels", "")
	if p := nf.json(t); p["type"] != "urn:kista:problem:not-found" || nf.header.Get("Content-Type") != "application/problem+json" {
		t.Errorf("problem: %v", p)
	}
	for _, path := range []string{"/api/v1/tenants/acme/channels/nope", "/api/v1/tenants/acme/channels/prod/nothing"} {
		if r := en.get(t, path, ""); r.status != 404 || !bytes.Equal(r.body, nf.body) {
			t.Errorf("%s: %d %s", path, r.status, r.body)
		}
	}
	// two Authorization headers, or another scheme: 401
	req = httptest.NewRequest("GET", "/api/v1/tenants/acme/channels", nil)
	req.Header.Add("Authorization", "Bearer "+en.idp.token(t, "alice"))
	req.Header.Add("Authorization", "Bearer "+en.idp.token(t, "alice"))
	rec = httptest.NewRecorder()
	en.serve.ServeHTTP(rec, serve.WithHTTPS(req))
	if rec.Code != 401 {
		t.Errorf("two Authorization headers: %d", rec.Code)
	}
	if r := en.get(t, "/api/v1/tenants/acme/channels", "", "Authorization", "Basic YTpi"); r.status != 401 {
		t.Errorf("basic: %d", r.status)
	}
	// whoami needs a token; a token holder's answer is private and has its own 304
	if r := en.get(t, "/api/v1/tenants/acme/whoami", ""); r.status != 401 {
		t.Errorf("anonymous whoami: %d", r.status)
	}
	tok := en.idp.token(t, "alice")
	w := en.get(t, "/api/v1/tenants/acme/channels/prod/extensions", tok)
	if w.header.Get("Cache-Control") != "private, no-cache" || w.header.Get("Vary") != "Authorization" {
		t.Errorf("token holder: %v", w.header)
	}
	if r := en.get(t, "/api/v1/tenants/acme/channels/prod/extensions", tok, "If-None-Match", `W/`+w.header.Get("ETag")); r.status != 304 {
		t.Errorf("weak If-None-Match: %d", r.status)
	}
	// limit and cursor
	for _, q := range []string{"limit=0", "limit=501", "limit=+5", "limit=x", "cursor=***", "duckdb_version=v2.0.0"} {
		if r := en.get(t, "/api/v1/tenants/acme/channels/prod/extensions?"+q, ""); r.status != 400 {
			t.Errorf("%s: %d", q, r.status)
		}
	}
	// holds_admin
	if m := en.get(t, "/api/v1/tenants/acme/whoami", tok).json(t); m["holds_admin"] != false || len(m["grants"].([]any)) != 0 {
		t.Errorf("whoami without grants: %v", m)
	}
	if _, err := en.adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"admin"}, "prod", ""); err != nil {
		t.Fatal(err)
	}
	if m := en.get(t, "/api/v1/tenants/acme/whoami", tok).json(t); m["holds_admin"] != true || len(m["grants"].([]any)) != 1 {
		t.Errorf("whoami with issuer admin: %v", m)
	}
	// a suspended tenant is 404 for everyone
	if _, err := en.ten.SetTenantState(ctx, admin, "acme", store.TenantSuspended); err != nil {
		t.Fatal(err)
	}
	if r := en.get(t, "/api/v1/tenants/acme/channels", tok); r.status != 404 || !bytes.Equal(r.body, nf.body) {
		t.Errorf("suspended: %d", r.status)
	}
}

// The limiter runs before token verification, per client address as kista serve computes it, with
// IPv6 grouped by /64.
func TestRateLimit(t *testing.T) {
	en := newEnv(t)
	h := serve.NewHandler(en.st, nil, nil, serve.Options{Log: slog.New(slog.DiscardHandler),
		API: api.New(api.Options{Store: en.st, Rate: 0.25, Burst: 2, Log: slog.New(slog.DiscardHandler)})})
	h.SetReady(true)
	call := func(addr, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/tenants/acme/channels", nil)
		req.RemoteAddr = addr
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, serve.WithHTTPS(req))
		return rec
	}
	for i, want := range []int{401, 401, 429} {
		if rec := call("[2001:db8::1]:1000", "garbage"); rec.Code != want {
			t.Fatalf("call %d: %d", i, rec.Code)
		}
	}
	// the same /64 from another port and address is the same client; Retry-After is 1/rate
	rec := call("[2001:db8::2]:2000", "")
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "4" {
		t.Fatalf("same /64: %d %v", rec.Code, rec.Header())
	}
	if rec := call("[2001:db8:0:1::1]:1000", ""); rec.Code != 200 {
		t.Fatalf("another /64: %d", rec.Code)
	}
}
