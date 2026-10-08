package serve

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

const (
	idpURL    = "https://idp.example"
	publicURL = "https://kista.example"
)

// fakeIDP serves discovery and a JWKS, and signs tokens.
type fakeIDP struct{ key *rsa.PrivateKey }

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

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIDP) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{"iss": idpURL, "sub": "alice", "aud": publicURL + "/acme", "exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(), "tid": "t1"}
	for k, v := range claims {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	hb, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	cb, _ := json.Marshal(c)
	input := b64u(hb) + "." + b64u(cb)
	d := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64u(sig)
}

func authEnv(t *testing.T, scheme string) (*env, *fakeIDP, *tenants.AuthAdmin) {
	t.Helper()
	en := newEnv(t, Options{}, scheme)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeIDP{key: key}
	en.h.o.Verifier, en.h.o.PublicURL = &auth.Verifier{Fetch: f}, publicURL
	adm := &tenants.AuthAdmin{Store: en.st, Authz: authz.ServerAdmin{}, Fetch: f, PublicURL: publicURL}
	if _, err := adm.AddIssuer(ctx, admin, "acme", store.Issuer{Name: "corp", URL: idpURL,
		RequiredClaims: map[string]string{"tid": "t1"}}); err != nil {
		t.Fatal(err)
	}
	return en, f, adm
}

func bearerHdr(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

func TestAnswerTable(t *testing.T) {
	en, idp, adm := authEnv(t, "https")
	pub := en.add(t, ext(t, 2000, 1, cpp("1.0")), release.AddOptions{Name: "tresor"})
	priv := en.add(t, ext(t, 2000, 2, cpp("1.1")), release.AddOptions{Name: "tresor", Private: true})
	tok := idp.token(t, nil)
	const (
		privPath = "/acme/prod/tresor/1.1/v2.0.0/linux_amd64/tresor.duckdb_extension.gz"
		pubPath  = "/acme/prod/tresor/1.0/v2.0.0/linux_amd64/tresor.duckdb_extension.gz"
		missing  = "/acme/prod/tresor/9.9/v2.0.0/linux_amd64/tresor.duckdb_extension.gz"
		flatPath = flat + "tresor.duckdb_extension.gz"
	)
	status := func(path string, hdr map[string]string) answer { return en.do(t, "GET", path, hdr) }

	// no token
	if a := status(pubPath, nil); a.status != 200 {
		t.Fatalf("public, no token: %d", a.status)
	}
	a401, b401 := status(privPath, nil), status(missing, nil)
	if a401.status != 401 || b401.status != 401 || !bytes.Equal(a401.body, b401.body) || !sameHeaders(a401.header, b401.header) {
		t.Fatalf("no token: %d %d", a401.status, b401.status)
	}
	// a valid token without a grant: 404, the same for private and missing
	a404, b404 := status(privPath, bearerHdr(tok)), status(missing, bearerHdr(tok))
	if a404.status != 404 || b404.status != 404 || !bytes.Equal(a404.body, b404.body) || !sameHeaders(a404.header, b404.header) {
		t.Fatalf("token without a grant: %d %d", a404.status, b404.status)
	}
	if a := status(pubPath, bearerHdr(tok)); a.status != 200 {
		t.Fatalf("public with a token: %d", a.status)
	}
	// current per caller: anonymous sees the public 1.0 on the flat path
	en.checkFile(t, status(flatPath, nil).body, true, pub)

	// grant install on the extension
	g, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, "", "tresor")
	if err != nil {
		t.Fatal(err)
	}
	a := status(privPath, bearerHdr(tok))
	if a.status != 200 || a.header.Get("Cache-Control") != "private, no-store, no-transform" || a.header.Get("Vary") != "Authorization" {
		t.Fatalf("granted: %d %v", a.status, a.header)
	}
	en.checkFile(t, a.body, true, priv)
	en.checkFile(t, status(flatPath, bearerHdr(tok)).body, true, priv) // the newer private release is current for this caller
	if a := status(missing, bearerHdr(tok)); a.status != 404 {
		t.Fatalf("granted, missing: %d", a.status)
	}
	// a token that is not valid is no token: public still served, private 401
	expired := idp.token(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix(), "iat": time.Now().Add(-2 * time.Hour).Unix()})
	if a := status(pubPath, bearerHdr(expired)); a.status != 200 {
		t.Fatalf("public with an expired token: %d", a.status)
	}
	if a := status(privPath, bearerHdr(expired)); a.status != 401 {
		t.Fatalf("private with an expired token: %d", a.status)
	}
	// another tenant's audience
	if a := status(privPath, bearerHdr(idp.token(t, map[string]any{"aud": publicURL + "/other"}))); a.status != 401 {
		t.Fatalf("another tenant's token: %d", a.status)
	}
	// the grant's removal applies at the next request
	if err := adm.RemoveGrant(ctx, admin, "acme", g.ID); err != nil {
		t.Fatal(err)
	}
	if a := status(privPath, bearerHdr(tok)); a.status != 404 {
		t.Fatalf("after the grant's removal: %d", a.status)
	}
	// admin on an issuer-wide grant is refused (spec 0007): every account of the issuer would hold
	// it; a row added before is ignored, and does not imply install
	if _, err := adm.AddGrant(ctx, admin, "acme", "issuer:corp", []string{"admin"}, "prod", ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("issuer-wide admin grant: %v", err)
	}
	iss, err := adm.ListIssuers(ctx, admin, "acme")
	if err != nil {
		t.Fatal(err)
	}
	ten, err := en.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := en.st.InTx(ctx, "", func(tx *store.Tx) error {
		return tx.InsertGrant(ctx, &store.Grant{TenantID: ten.ID, IssuerID: iss[0].ID, Kind: store.PrincipalIssuer, Verbs: []string{"admin"}})
	}); err != nil {
		t.Fatal(err)
	}
	if a := status(privPath, bearerHdr(idp.token(t, map[string]any{"sub": "bob"}))); a.status != 404 {
		t.Fatalf("issuer-wide admin: %d", a.status)
	}
	// an issuer-wide grant needs required claims, which this record has
	if _, err := adm.AddGrant(ctx, admin, "acme", "issuer:corp", []string{"install"}, "prod", ""); err != nil {
		t.Fatal(err)
	}
	if a := status(privPath, bearerHdr(idp.token(t, map[string]any{"sub": "bob"}))); a.status != 200 {
		t.Fatalf("issuer-wide grant: %d", a.status)
	}
	// removing the issuer removes its grants; a new record under the same name starts with none
	if err := adm.RemoveIssuer(ctx, admin, "acme", "corp", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddIssuer(ctx, admin, "acme", store.Issuer{Name: "corp", URL: idpURL}); err != nil {
		t.Fatal(err)
	}
	if a := status(privPath, bearerHdr(tok)); a.status != 404 {
		t.Fatalf("a re-added issuer inherited grants: %d", a.status)
	}
	if _, err := adm.AddGrant(ctx, admin, "acme", "issuer:corp", []string{"install"}, "", ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("an issuer: grant without required claims: %v", err)
	}
	// assigned audiences: unique, never under the public URL
	if err := adm.AddAudience(ctx, admin, "acme", "api://kista-acme"); err != nil {
		t.Fatal(err)
	}
	if err := adm.AddAudience(ctx, admin, "acme", publicURL+"/other"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a canonical audience assigned: %v", err)
	}
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if a := status(privPath, bearerHdr(idp.token(t, map[string]any{"aud": "api://kista-acme"}))); a.status != 200 {
		t.Fatalf("the assigned audience: %d", a.status)
	}
	// an audience under the public URL written to the store directly (another tenant's canonical one)
	// is never honoured
	tn, _ := en.st.GetTenant(ctx, "acme")
	if err := en.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.AddAudience(ctx, tn.ID, publicURL+"/beta", "test") }); err != nil {
		t.Fatal(err)
	}
	if a := status(privPath, bearerHdr(idp.token(t, map[string]any{"aud": publicURL + "/beta"}))); a.status != 401 {
		t.Fatalf("an audience under the public URL was honoured: %d", a.status)
	}
}

// Over plain http a token is never used.
func TestTokenOverHTTP(t *testing.T) {
	en, idp, adm := authEnv(t, "http")
	en.add(t, ext(t, 2000, 2, cpp("1.1")), release.AddOptions{Name: "tresor", Private: true})
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if a := en.do(t, "GET", "/acme/prod/tresor/1.1/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", bearerHdr(idp.token(t, nil))); a.status != 401 {
		t.Fatalf("a token over http: %d", a.status)
	}
}
