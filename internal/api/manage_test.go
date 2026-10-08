package api_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

const (
	opsURL      = "https://ops.example"
	serverAud   = "api://kista"
	otherIdPURL = "https://other-idp.example"
	ghURL       = "https://token.actions.example"
)

// idps serves discovery and JWKS for several issuers.
type idps map[string]*rsa.PrivateKey

func (m idps) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	for iss, key := range m {
		switch url {
		case iss + "/.well-known/openid-configuration":
			return []byte(`{"issuer":"` + iss + `","jwks_uri":"` + iss + `/jwks"}`), http.Header{}, nil
		case iss + "/jwks":
			b, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
				"n": b64u(key.N.Bytes()), "e": b64u(big.NewInt(int64(key.E)).Bytes())}}})
			return b, http.Header{}, nil
		}
	}
	return nil, nil, errors.New("not found")
}

func (m idps) sign(t *testing.T, iss string, claims map[string]any) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{"iss": iss, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range claims {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return b64u(b) }
	input := enc(map[string]any{"alg": "RS256", "kid": "k1"}) + "." + enc(c)
	d := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, m[iss], crypto.SHA256, d[:])
	return input + "." + b64u(sig)
}

type mgmt struct {
	*env
	keys   idps
	h      *serve.Handler
	ten    *tenants.Service
	adm    *tenants.AuthAdmin
	toks   map[string]string // caller -> token
	other  string            // a grant id in tenant other
	keySvc *keys.Service     // the API's key service
}

// newMgmt builds the API with server identity and management: server issuer ops (role
// kista.admin is a server administrator), tenant acme (issuer corp) and tenant other (issuer
// corp, at another IdP).
func newMgmt(t *testing.T) *mgmt {
	t.Helper()
	en := newEnv(t)
	ks := idps{idpURL: en.idp.key, opsURL: mustKey(t), otherIdPURL: mustKey(t), ghURL: mustKey(t)}
	providers := auth.Providers{{Name: "github", URL: ghURL, Verifier: &auth.Verifier{Fetch: ks}}}
	server := &auth.Server{
		Issuers: []store.Issuer{{ID: auth.ServerIssuerID("ops"), Name: "ops", URL: opsURL, Algorithms: auth.Algorithms,
			RequiredClaims: map[string]string{"tid": "t1"}, RolesClaim: []string{"roles"}, MaxTokenLifetime: 24 * time.Hour}},
		Audiences: []string{serverAud},
		Admins:    auth.Principals{{IssuerID: auth.ServerIssuerID("ops"), Kind: store.PrincipalRole, Value: "kista.admin"}: true},
		Verifier:  &auth.Verifier{Fetch: ks},
	}
	auths := &auth.TenantAuths{Store: en.st}
	grants := authz.Grants{Store: en.st, Auths: auths}
	ten := &tenants.Service{Store: en.st, Authz: grants, HasDomain: func(d string) bool { return d == "default" }}
	adm := &tenants.AuthAdmin{Store: en.st, Authz: grants, Fetch: ks, PublicURL: publicURL, ServerAudiences: []string{serverAud},
		Providers: providers}
	keySvc, relSvc := *en.keys, *en.rel
	keySvc.Authz, relSvc.Authz, relSvc.Signers = grants, grants, &keySvc
	eg, err := egress.New(egress.Config{AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ups := &upstream.Service{Store: en.st, Releases: &relSvc, Blob: en.blob, Fetch: eg, Authz: grants, MaxBody: 1 << 20, MaxIngests: 4,
		TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	apiH := api.New(api.Options{Store: en.st, Snapshots: &release.Snapshots{Store: en.st}, Auths: auths,
		Verifier: &auth.Verifier{Fetch: ks}, PublicURL: publicURL, Rate: 1000, Burst: 1000, Log: slog.New(slog.DiscardHandler),
		Server: server, Providers: providers, Authz: grants, Tenants: ten, Auth: adm, Keys: &keySvc, Releases: &relSvc, Upstreams: ups})
	h := serve.NewHandler(en.st, en.keys, en.blob, serve.Options{Log: slog.New(slog.DiscardHandler), Verifier: &auth.Verifier{Fetch: ks},
		Server: server, PublicURL: publicURL, Auths: auths, API: apiH, MaxDownloads: 8, MaxDownloadsPerClient: 8, MinRate: 1024,
		WriteIdleTimeout: 10 * time.Second})
	h.SetReady(true)
	m := &mgmt{env: en, keys: ks, h: h, ten: ten, adm: adm, toks: map[string]string{}, keySvc: &keySvc}

	// the CLI sets up: tenant other, grants in acme
	cli := en.adm
	cli.Fetch = ks
	if _, err := en.ten.CreateTenant(ctx, admin, "other", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.AddIssuer(ctx, admin, "other", store.Issuer{Name: "corp", URL: otherIdPURL}); err != nil {
		t.Fatal(err)
	}
	og, err := cli.AddGrant(ctx, admin, "other", "subject:corp|olga", []string{"admin"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	m.other = og.ID
	for _, g := range []struct{ sub, channel, ext, verb string }{
		{"ivan", "", "", "install"}, {"eve", "prod", "tresor", "admin"}, {"carl", "prod", "", "admin"}, {"tom", "", "", "admin"},
	} {
		if _, err := cli.AddGrant(ctx, admin, "acme", "subject:corp|"+g.sub, []string{g.verb}, g.channel, g.ext); err != nil {
			t.Fatal(err)
		}
	}
	acme := func(sub string) string {
		return ks.sign(t, idpURL, map[string]any{"sub": sub, "aud": publicURL + "/acme"})
	}
	m.toks = map[string]string{
		"anonymous":    "",
		"no grants":    acme("nobody"),
		"install":      acme("ivan"),
		"ext admin":    acme("eve"),
		"channel adm":  acme("carl"),
		"tenant admin": acme("tom"),
		"other admin":  ks.sign(t, otherIdPURL, map[string]any{"sub": "olga", "aud": publicURL + "/other"}),
		"server admin": ks.sign(t, opsURL, map[string]any{"sub": "root", "aud": serverAud, "tid": "t1", "roles": []any{"kista.admin"}}),
		"server user":  ks.sign(t, opsURL, map[string]any{"sub": "joe", "aud": serverAud, "tid": "t1"}),
		"stale admin": ks.sign(t, idpURL, map[string]any{"sub": "tom", "aud": publicURL + "/acme",
			"iat": time.Now().Add(-2 * time.Hour).Unix()}),
		"stale server": ks.sign(t, opsURL, map[string]any{"sub": "root", "aud": serverAud, "tid": "t1", "roles": []any{"kista.admin"},
			"iat": time.Now().Add(-2 * time.Hour).Unix()}),
	}
	return m
}

func mustKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (m *mgmt) call(t *testing.T, method, path, token, body string, hdr ...string) resp {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if body == "" {
		req.ContentLength = 0
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	m.h.ServeHTTP(rec, serve.WithHTTPS(req))
	return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
}

// Every phase-2 route × caller: who gets the answer, who 401, who 404 (with constant bytes).
func TestManagementAccess(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	type want map[string]int // caller -> status; unspecified: 404, or 401 for a caller without a valid token
	ok := func(code int, callers ...string) want {
		w := want{}
		for _, c := range callers {
			w[c] = code
		}
		return w
	}
	tenantAdmins := []string{"tenant admin", "server admin"}
	gs, err := m.env.adm.ListGrants(ctx, admin, "acme")
	if err != nil {
		t.Fatal(err)
	}
	grantID := gs[0].ID
	everyone := []string{"anonymous", "no grants", "install", "ext admin", "channel adm", "tenant admin", "server admin",
		"server user", "stale admin", "stale server"}
	cases := []struct {
		method, path, body string
		want               want
		hdr                []string
	}{
		{"GET", "/api/v1/tenants", "", ok(200, "server admin"), nil},
		{"GET", T, "", ok(200, tenantAdmins...), nil},
		{"GET", T + "/audiences", "", ok(200, tenantAdmins...), nil},
		{"GET", T + "/issuers", "", ok(200, tenantAdmins...), nil},
		{"GET", T + "/issuers/corp", "", ok(200, tenantAdmins...), nil},
		{"GET", T + "/grants", "", ok(200, tenantAdmins...), nil},
		{"POST", T + "/audiences", `{"audience":"api://acme"}`, ok(200, "server admin"), nil},
		{"POST", "/api/v1/duckdb-versions", `{"name":"v2.1.0","kind":"release","c_api_maxima":["v1.5.6"]}`, ok(201, "server admin"), nil},
		{"POST", T + "/grants", `{"principal":"subject:corp|zed","verbs":["install"]}`, ok(201, tenantAdmins...), nil},
		{"GET", "/api/v1/duckdb-versions", "", ok(200, append(everyone, "other admin")...), nil},
		{"GET", "/api/v1/duckdb-versions/v2.0.0", "", ok(200, append(everyone, "other admin")...), nil},
		{"GET", T + "/channels/prod/extensions", "", ok(200, everyone...), nil},
		{"GET", T + "/grants/" + grantID, "", ok(200, tenantAdmins...), nil},
		{"POST", "/api/v1/duckdb-versions/v2.0.0/c-apis", `{"c_api":"v3.0.0"}`, ok(200, "server admin"), nil},
		{"POST", T + "/audiences/remove", `{"audience":"api://acme"}`, ok(200, "server admin"), nil},
		{"POST", "/api/v1/tenants/other/suspend", "", ok(200, "server admin"), []string{"If-Match", `"v1"`}},
	}
	var ref401, ref404 *resp
	// the server administrator goes last: its writes (a suspension) do not change what the others get
	callers := slices.Sorted(maps.Keys(m.toks))
	callers = append(slices.DeleteFunc(callers, func(c string) bool { return c == "server admin" }), "server admin")
	for _, tc := range cases {
		for _, caller := range callers {
			tok := m.toks[caller]
			// a write with the same body twice would conflict: use a fresh grant principal per caller
			b := strings.ReplaceAll(tc.body, "zed", strings.ReplaceAll(caller, " ", "-"))
			r := m.call(t, tc.method, tc.path, tok, b, tc.hdr...)
			exp, set := tc.want[caller]
			if !set {
				_, tenantAdminMay := tc.want["tenant admin"]
				switch {
				case caller == "anonymous":
					exp = 401
				case caller == "other admin" && strings.HasPrefix(tc.path, T):
					exp = 401 // another tenant's token is not valid for this tenant
				case strings.HasPrefix(tc.path, "/api/v1/tenants/other") && caller != "other admin" && !strings.HasPrefix(caller, "server") &&
					caller != "stale server":
					exp = 401 // nor an acme token for tenant other
				case caller == "stale admin" && tenantAdminMay:
					exp = 401 // management needs a recently issued token
				case caller == "stale server" && tc.want["server admin"] != 0:
					exp = 401
				default:
					exp = 404
				}
			}
			if r.status != exp {
				t.Errorf("%s %s as %s: %d, want %d: %s", tc.method, tc.path, caller, r.status, exp, r.body)
				continue
			}
			ref := &ref401
			if r.status == 404 {
				ref = &ref404
			}
			if r.status == 401 || r.status == 404 {
				if *ref == nil {
					*ref = &r
				} else if !bytes.Equal(r.body, (*ref).body) || !sameHeaders(r.header, (*ref).header) {
					t.Errorf("%s %s as %s: %d bytes or headers differ: %v %v", tc.method, tc.path, caller, r.status, r.header, (*ref).header)
				}
			}
			if r.status < 300 && tc.path != T+"/channels/prod/extensions" && !strings.HasPrefix(tc.path, "/api/v1/duckdb-versions") &&
				r.header.Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s: Cache-Control %q", tc.method, tc.path, r.header.Get("Cache-Control"))
			}
		}
	}
	// a stale token still reads the index, not management
	if r := m.call(t, "GET", T+"/whoami", m.toks["stale admin"], ""); r.status != 200 {
		t.Errorf("stale token on whoami: %d", r.status)
	}
	// /api/v1/whoami: a server token's identity; a tenant token is told it is not one
	if r := m.call(t, "GET", "/api/v1/whoami", m.toks["server admin"], ""); r.status != 200 || r.json(t)["administrator"] != true {
		t.Errorf("server whoami: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/whoami", m.toks["server user"], ""); r.status != 200 || r.json(t)["administrator"] != false {
		t.Errorf("server user whoami: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/whoami", m.toks["tenant admin"], ""); r.status != 400 {
		t.Errorf("tenant token on server whoami: %d", r.status)
	}
	// a server administrator sees every release in the index; storage domains are its alone
	if got := names(m.call(t, "GET", T+"/channels/prod/extensions", m.toks["server admin"], "").json(t), "extensions", "name"); len(got) != 3 {
		t.Errorf("server admin's index: %v", got)
	}
	if r := m.call(t, "GET", T, m.toks["server admin"], "").json(t); r["storage_domain"] != "default" {
		t.Errorf("server admin's tenant: %v", r)
	}
	if r := m.call(t, "GET", T, m.toks["tenant admin"], "").json(t); r["storage_domain"] != nil {
		t.Errorf("tenant admin sees the storage domain: %v", r)
	}
}

// Server and tenant identities never mix.
func TestServerIdentity(t *testing.T) {
	m := newMgmt(t)
	// a tenant record named like the server issuer, at the server issuer's URL, with a role that
	// server_admins names: its principals carry the record's id, never server:ops
	if _, err := m.env.adm.AddIssuer(ctx, admin, "acme", store.Issuer{Name: "ops", URL: opsURL,
		RequiredClaims: map[string]string{"tid": "t1"}, RolesClaim: []string{"roles"}}); err != nil {
		t.Fatal(err)
	}
	tenantTok := m.keys.sign(t, opsURL, map[string]any{"sub": "x", "aud": publicURL + "/acme", "tid": "t1", "roles": []any{"kista.admin"}})
	if r := m.call(t, "GET", "/api/v1/tenants", tenantTok, ""); r.status != 404 {
		t.Errorf("a tenant record's admin role on a server route: %d", r.status)
	}
	if r := m.call(t, "GET", "/api/v1/tenants/acme/grants", tenantTok, ""); r.status != 404 {
		t.Errorf("a tenant record's admin role without a grant: %d", r.status)
	}
	// an aud array mixing a server and a tenant audience is a server token only: corp is not a
	// server issuer, so it is not valid, and the DuckDB routes see no token
	mixed := m.keys.sign(t, idpURL, map[string]any{"sub": "tom", "aud": []any{serverAud, publicURL + "/acme"}})
	if r := m.call(t, "GET", "/api/v1/tenants/acme/grants", mixed, ""); r.status != 401 {
		t.Errorf("a mixed audience: %d", r.status)
	}
	// ivan holds install on everything in acme: with the server audience beside acme's, the DuckDB
	// routes take the token as none
	ivanMixed := m.keys.sign(t, idpURL, map[string]any{"sub": "ivan", "aud": []any{serverAud, publicURL + "/acme"}})
	ivan := m.keys.sign(t, idpURL, map[string]any{"sub": "ivan", "aud": publicURL + "/acme"})
	const aclPath = "/acme/prod/acl/1.0/v2.0.0/linux_amd64/acl.duckdb_extension.gz"
	if r := m.call(t, "GET", aclPath, ivan, ""); r.status != 200 {
		t.Fatalf("ivan's own token on a private extension: %d", r.status)
	}
	if r := m.call(t, "GET", aclPath, ivanMixed, ""); r.status != 401 {
		t.Errorf("a token with a server audience on a DuckDB route: %d (want 401: no token)", r.status)
	}
	// a valid tenant token, aud as an array, on a server route
	arr := m.keys.sign(t, idpURL, map[string]any{"sub": "tom", "aud": []any{publicURL + "/acme"}})
	if r := m.call(t, "GET", "/api/v1/tenants", arr, ""); r.status != 404 {
		t.Errorf("a tenant token on a server route: %d", r.status)
	}
	// a server audience cannot be assigned to a tenant
	if r := m.call(t, "POST", "/api/v1/tenants/acme/audiences", m.toks["server admin"], `{"audience":"`+serverAud+`"}`); r.status != 400 {
		t.Errorf("a server audience to a tenant: %d", r.status)
	}
	// ids from another tenant are not found
	if r := m.call(t, "GET", "/api/v1/tenants/acme/grants/"+m.other, m.toks["tenant admin"], ""); r.status != 404 {
		t.Errorf("another tenant's grant id: %d", r.status)
	}
	if r := m.call(t, "DELETE", "/api/v1/tenants/acme/grants/"+m.other, m.toks["tenant admin"], ""); r.status != 404 {
		t.Errorf("deleting another tenant's grant: %d", r.status)
	}
	// a write needs a named writer
	unnamed := m.keys.sign(t, opsURL, map[string]any{"aud": serverAud, "tid": "t1", "roles": []any{"kista.admin"}, "sub": nil})
	if r := m.call(t, "POST", "/api/v1/tenants", unnamed, `{"name":"x1"}`); r.status != 401 {
		t.Errorf("an unnamed writer: %d", r.status)
	}
	if r := m.call(t, "GET", "/api/v1/tenants", unnamed, ""); r.status != 200 {
		t.Errorf("an unnamed reader: %d", r.status)
	}
}

func TestManagementWrites(t *testing.T) {
	m := newMgmt(t)
	sa, ta := m.toks["server admin"], m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"

	// tenants: create, duplicate, suspend with If-Match
	r := m.call(t, "POST", "/api/v1/tenants", sa, `{"name":"beta","display_name":"Beta"}`)
	if r.status != 201 || r.header.Get("Location") != "/api/v1/tenants/beta" || r.header.Get("ETag") != `"v1"` {
		t.Fatalf("create tenant: %d %v %s", r.status, r.header, r.body)
	}
	if r := m.call(t, "POST", "/api/v1/tenants", sa, `{"name":"beta"}`); r.status != 409 {
		t.Errorf("duplicate tenant: %d", r.status)
	}
	if r := m.call(t, "POST", "/api/v1/tenants", sa, `{"name":"Bad Name"}`); r.status != 400 {
		t.Errorf("invalid tenant: %d", r.status)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/beta/suspend", sa, ""); r.status != 428 {
		t.Errorf("suspend without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/beta/suspend", sa, "", "If-Match", `"v9"`); r.status != 412 {
		t.Errorf("suspend with a stale If-Match: %d", r.status)
	}
	r = m.call(t, "POST", "/api/v1/tenants/beta/suspend", sa, "", "If-Match", `"v1"`)
	if r.status != 200 || r.json(t)["state"] != store.TenantSuspended || r.header.Get("ETag") != `"v2"` {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
	// a suspended tenant is still there for server administrators
	if r := m.call(t, "GET", "/api/v1/tenants/beta", sa, ""); r.status != 200 {
		t.Errorf("suspended tenant, server admin: %d", r.status)
	}

	// issuers: add (discovery runs), get with its ETag, delete with If-Match
	r = m.call(t, "POST", T+"/issuers", ta, `{"name":"ops2","url":"`+opsURL+`","required_claims":{"tid":"t1"},"roles_claim":["roles"]}`)
	if r.status != 201 || r.header.Get("ETag") == "" {
		t.Fatalf("add issuer: %d %s", r.status, r.body)
	}
	tag := r.header.Get("ETag")
	if r := m.call(t, "POST", T+"/issuers", ta, `{"name":"bad","url":"https://unknown.example"}`); r.status != 400 ||
		!strings.Contains(string(r.body), "discovery") {
		t.Errorf("an issuer that cannot be fetched: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/issuers/ops2", ta, ""); r.status != 428 {
		t.Errorf("delete issuer without If-Match: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/issuers/ops2", ta, "", "If-Match", `"nope"`); r.status != 412 {
		t.Errorf("delete issuer with another id: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/issuers/ops2", ta, "", "If-Match", tag); r.status != 204 {
		t.Errorf("delete issuer: %d %s", r.status, r.body)
	}

	// grants: issuer: with admin refused; a reference to a missing channel is 400; Location
	if r := m.call(t, "POST", T+"/grants", ta, `{"principal":"issuer:corp","verbs":["admin"]}`); r.status != 400 {
		t.Errorf("issuer: admin grant: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/grants", ta, `{"principal":"subject:corp|zed","verbs":["install"],"channel":"nope"}`); r.status != 400 {
		t.Errorf("a grant on a missing channel: %d", r.status)
	}
	r = m.call(t, "POST", T+"/grants", ta, `{"principal":"subject:corp|zed","verbs":["install"],"channel":"prod"}`)
	if r.status != 201 || !strings.HasPrefix(r.header.Get("Location"), T+"/grants/") {
		t.Fatalf("add grant: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/grants", ta, `{"principal":"subject:corp|zed","verbs":["install"],"channel":"prod"}`); r.status != 409 {
		t.Errorf("the same grant twice: %d", r.status)
	}
	if got := m.call(t, "GET", T+"/grants?principal=subject:corp|zed", ta, "").json(t)["grants"].([]any); len(got) != 1 {
		t.Errorf("grants by principal: %v", got)
	}

	// lock-out: tom cannot remove the last tenant-wide admin grant (his own), nor the issuer
	// holding it; a server administrator can
	gs := m.call(t, "GET", T+"/grants?principal=subject:corp|tom", ta, "").json(t)["grants"].([]any)
	tomGrant := gs[0].(map[string]any)["id"].(string)
	if r := m.call(t, "DELETE", T+"/grants/"+tomGrant, ta, ""); r.status != 409 {
		t.Errorf("removing the last admin grant: %d %s", r.status, r.body)
	}
	corp := m.call(t, "GET", T+"/issuers/corp", ta, "")
	if r := m.call(t, "DELETE", T+"/issuers/corp", ta, "", "If-Match", corp.header.Get("ETag")); r.status != 409 {
		t.Errorf("removing the issuer of the last admin grant: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/grants/"+tomGrant, sa, ""); r.status != 204 {
		t.Errorf("a server admin removes the last admin grant: %d", r.status)
	}

	// pagination of grants
	p1 := m.call(t, "GET", T+"/grants?limit=2", sa, "").json(t)
	if len(p1["grants"].([]any)) != 2 || p1["next"] == nil {
		t.Fatalf("grants page 1: %v", p1)
	}
	p2 := m.call(t, "GET", T+"/grants?limit=2&cursor="+p1["next"].(string), sa, "").json(t)
	if len(p2["grants"].([]any)) == 0 || p2["grants"].([]any)[0].(map[string]any)["id"] == p1["grants"].([]any)[1].(map[string]any)["id"] {
		t.Errorf("grants page 2: %v", p2)
	}
}

func TestManagementBodies(t *testing.T) {
	m := newMgmt(t)
	sa := m.toks["server admin"]
	post := func(body, ctype string) resp {
		req := httptest.NewRequest("POST", "/api/v1/tenants", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+sa)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		rec := httptest.NewRecorder()
		m.h.ServeHTTP(rec, serve.WithHTTPS(req))
		return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
	}
	for _, tc := range []struct {
		body, ctype string
		want        int
	}{
		{`{"name":"a1"}`, "text/plain", 415},
		{`{"name":"a1"}`, "", 415},
		{`{"name":"a1"}`, "application/json; charset=latin1", 415},
		{`{"name":"a1","x":1}`, "application/json", 400},
		{`{"name":"a1","name":"a2"}`, "application/json", 400},
		{`{"name":"a1"} {}`, "application/json", 400},
		{`{"name":` + strings.Repeat(" ", 70<<10) + `"a1"}`, "application/json", 413},
		{`{"name":"a3"}`, "application/json; charset=utf-8", 201},
	} {
		if r := post(tc.body, tc.ctype); r.status != tc.want {
			t.Errorf("%.40q (%s): %d, want %d: %s", tc.body, tc.ctype, r.status, tc.want, r.body)
		}
	}
	// a body is read only after the decision: a caller who may not write gets 404 whatever it sends
	req := httptest.NewRequest("POST", "/api/v1/tenants", strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer "+m.toks["tenant admin"])
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	m.h.ServeHTTP(rec, serve.WithHTTPS(req))
	if rec.Code != 404 {
		t.Errorf("a refused caller's body: %d", rec.Code)
	}
	// 405 lists the route's methods
	if r := m.call(t, "DELETE", "/api/v1/tenants", sa, ""); r.status != 405 || r.header.Get("Allow") != "GET, HEAD, POST" {
		t.Errorf("405: %d %v", r.status, r.header)
	}
}

func sameHeaders(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if strings.Join(a.Values(k), "\n") != strings.Join(b.Values(k), "\n") {
			return false
		}
	}
	return true
}

// slow holds discovery of one issuer URL until released.
type slow struct {
	idps
	url  string
	gate chan struct{}
	in   chan struct{}
}

func (s slow) Get(ctx context.Context, url string) ([]byte, http.Header, error) {
	if strings.HasPrefix(url, s.url) {
		s.in <- struct{}{}
		<-s.gate
	}
	return s.idps.Get(ctx, url)
}

func TestManagementDetails(t *testing.T) {
	m := newMgmt(t)
	sa, ta := m.toks["server admin"], m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"

	// lock-out allows removing an admin grant while another tenant-wide one remains
	r := m.call(t, "POST", T+"/grants", ta, `{"principal":"subject:corp|amy","verbs":["admin"]}`)
	if r.status != 201 {
		t.Fatalf("second admin: %d %s", r.status, r.body)
	}
	amy := r.json(t)["id"].(string)
	if r := m.call(t, "DELETE", T+"/grants/"+amy, ta, ""); r.status != 204 || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("removing a non-last admin grant: %d %v", r.status, r.header)
	}

	// a server administrator's identity is shown to server administrators only
	r = m.call(t, "POST", T+"/grants", sa, `{"principal":"subject:corp|bob","verbs":["install"]}`)
	if r.status != 201 || r.json(t)["created_by"] != "server:ops|root" {
		t.Fatalf("grant by a server admin: %d %s", r.status, r.body)
	}
	if g := m.call(t, "GET", r.header.Get("Location"), ta, "").json(t); g["created_by"] != "server" {
		t.Errorf("a tenant admin sees %v", g["created_by"])
	}

	// an audience that is not there is a body reference: 400
	if r := m.call(t, "POST", T+"/audiences/remove", sa, `{"audience":"api://nope"}`); r.status != 400 {
		t.Errorf("removing a missing audience: %d", r.status)
	}

	// If-Match: * means any version; a list is refused
	if r := m.call(t, "POST", "/api/v1/tenants/other/suspend", sa, "", "If-Match", `"v1", "v2"`); r.status != 400 {
		t.Errorf("an If-Match list: %d", r.status)
	}
	if r := m.call(t, "POST", "/api/v1/tenants/other/suspend", sa, "", "If-Match", "*"); r.status != 200 {
		t.Errorf("If-Match *: %d", r.status)
	}
	// suspend reads no body
	if r := m.call(t, "POST", "/api/v1/tenants/other/resume", sa, `{}`, "If-Match", "*"); r.status != 400 {
		t.Errorf("a body on resume: %d", r.status)
	}

	// issuers page in byte order of their names, whatever order the database returns
	for _, n := range []string{"ab", "a-c", "b"} {
		if _, err := m.env.adm.AddIssuer(ctx, admin, "acme", store.Issuer{Name: n, URL: "https://" + n + ".example", JWKSURI: opsURL + "/jwks"}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	cursor := ""
	for range 10 {
		pg := m.call(t, "GET", T+"/issuers?limit=1&cursor="+cursor, ta, "").json(t)
		seen = append(seen, names(pg, "issuers", "name")...)
		next, _ := pg["next"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(seen, ",") != "a-c,ab,b,corp" {
		t.Errorf("issuer pages: %v", seen)
	}

	// adding issuers runs one at a time per tenant
	s := slow{idps: m.keys, url: "https://slow.example", gate: make(chan struct{}), in: make(chan struct{}, 1)}
	m.keys["https://slow.example"] = mustKey(t)
	m.adm.Fetch = s
	done := make(chan int)
	go func() {
		done <- m.call(t, "POST", T+"/issuers", ta, `{"name":"slow","url":"https://slow.example"}`).status
	}()
	<-s.in
	if r := m.call(t, "POST", T+"/issuers", ta, `{"name":"slow2","url":"https://slow.example"}`); r.status != 429 {
		t.Errorf("a second issuer add while one runs: %d", r.status)
	}
	close(s.gate)
	if st := <-done; st != 201 {
		t.Errorf("the first issuer add: %d", st)
	}
}
