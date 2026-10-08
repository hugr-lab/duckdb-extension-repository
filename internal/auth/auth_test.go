package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

var ctx = context.Background()

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jsonEnc(v any) string {
	b, _ := json.Marshal(v)
	return enc(b)
}

// idp is a fake issuer: keys, a JWKS and a discovery document served by fetch.
type idp struct {
	url   string
	rsa   *rsa.PrivateKey
	ec    *ecdsa.PrivateKey
	ed    ed25519.PrivateKey
	extra []map[string]any
	gets  atomic.Int32
}

func newIDP(t *testing.T, url string) *idp {
	t.Helper()
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	return &idp{url: url, rsa: rk, ec: ek, ed: ed}
}

func (p *idp) jwks() string {
	ecb, _ := p.ec.PublicKey.Bytes()
	keys := []map[string]any{
		{"kty": "RSA", "kid": "r1", "use": "sig", "n": enc(p.rsa.N.Bytes()), "e": enc(big.NewInt(int64(p.rsa.E)).Bytes())},
		{"kty": "EC", "kid": "e1", "crv": "P-256", "x": enc(ecb[1:33]), "y": enc(ecb[33:])},
		{"kty": "OKP", "kid": "d1", "crv": "Ed25519", "x": enc(p.ed.Public().(ed25519.PublicKey))},
	}
	keys = append(keys, p.extra...)
	b, _ := json.Marshal(map[string]any{"keys": keys})
	return string(b)
}

func (p *idp) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	p.gets.Add(1)
	switch url {
	case p.url + "/.well-known/openid-configuration":
		return []byte(`{"issuer":"` + p.url + `","jwks_uri":"` + p.url + `/jwks"}`), http.Header{}, nil
	case p.url + "/jwks":
		return []byte(p.jwks()), http.Header{"Cache-Control": {"max-age=3600"}}, nil
	}
	return nil, nil, errors.New("not found")
}

func (p *idp) sign(t *testing.T, alg, kid string, hdr map[string]any, claims map[string]any) string {
	t.Helper()
	h := map[string]any{"alg": alg, "kid": kid, "typ": "JWT"}
	for k, v := range hdr {
		if v == nil {
			delete(h, k)
		} else {
			h[k] = v
		}
	}
	input := jsonEnc(h) + "." + jsonEnc(claims)
	digest := sha256.Sum256([]byte(input))
	var sig []byte
	var err error
	switch alg {
	case "RS256":
		sig, err = rsa.SignPKCS1v15(rand.Reader, p.rsa, crypto.SHA256, digest[:])
	case "PS256":
		sig, err = rsa.SignPSS(rand.Reader, p.rsa, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case "ES256":
		r, s, e := ecdsa.Sign(rand.Reader, p.ec, digest[:])
		err = e
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	case "EdDSA":
		sig = ed25519.Sign(p.ed, []byte(input))
	case "HS256": // keyed with the RSA public key: the classic confusion attack
		der, _ := x509.MarshalPKIXPublicKey(&p.rsa.PublicKey)
		m := hmac.New(sha256.New, der)
		m.Write([]byte(input))
		sig = m.Sum(nil)
	case "none":
	}
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + enc(sig)
}

const canonical = "https://kista.example/acme"

func setup(t *testing.T) (*idp, *Verifier, store.TenantAuth, func(map[string]any) map[string]any) {
	p := newIDP(t, "https://idp.example")
	now := time.Unix(1_800_000_000, 0)
	v := &Verifier{Fetch: p, Now: func() time.Time { return now }}
	ta := store.TenantAuth{
		Issuers: []store.Issuer{{ID: "iss-1", Name: "corp", URL: p.url, Algorithms: Algorithms,
			RequiredClaims: map[string]string{"tid": "t1"}, RolesClaim: []string{"realm_access", "roles"},
			GroupsClaim: []string{"https://example.com/groups"}, ClientClaim: []string{"azp"}, MaxTokenLifetime: 2 * time.Hour}},
		Audiences: []string{"api://kista-acme"},
	}
	claims := func(over map[string]any) map[string]any {
		c := map[string]any{"iss": p.url, "sub": "alice", "aud": canonical, "exp": now.Add(time.Hour).Unix(),
			"iat": now.Add(-time.Minute).Unix(), "tid": "t1", "azp": "cli",
			"realm_access": map[string]any{"roles": []any{"dev", 7, "ops"}}, "https://example.com/groups": "g1"}
		for k, val := range over {
			if val == nil {
				delete(c, k)
			} else {
				c[k] = val
			}
		}
		return c
	}
	return p, v, ta, claims
}

func TestVerify(t *testing.T) {
	p, v, ta, claims := setup(t)
	for _, alg := range []string{"RS256", "PS256", "ES256", "EdDSA"} {
		kid := map[string]string{"RS256": "r1", "PS256": "r1", "ES256": "e1", "EdDSA": "d1"}[alg]
		pr, name, err := v.Verify(ctx, ta, canonical, p.sign(t, alg, kid, nil, claims(nil)))
		if err != nil || name != "corp" {
			t.Fatalf("%s: %v", alg, err)
		}
		for _, k := range []Key{{"iss-1", "issuer", ""}, {"iss-1", "subject", "alice"}, {"iss-1", "role", "dev"},
			{"iss-1", "role", "ops"}, {"iss-1", "group", "g1"}, {"iss-1", "client", "cli"}} {
			if !pr[k] {
				t.Fatalf("%s: missing %v in %v", alg, k, pr)
			}
		}
		if len(pr) != 6 {
			t.Fatalf("%s: unexpected principals %v", alg, pr)
		}
	}
	// the assigned audience, and an array
	if _, _, err := v.Verify(ctx, ta, canonical, p.sign(t, "RS256", "r1", nil, claims(map[string]any{"aud": []any{"x", "api://kista-acme"}}))); err != nil {
		t.Fatalf("assigned audience: %v", err)
	}
	now := v.Now()
	for name, tc := range map[string]struct {
		tok    string
		reason string
	}{
		"alg none":             {p.sign(t, "none", "r1", nil, claims(nil)), ReasonSignature},
		"HS256 with the key":   {p.sign(t, "HS256", "r1", nil, claims(nil)), ReasonSignature},
		"ES256 with RSA kid":   {p.sign(t, "ES256", "r1", nil, claims(nil)), ReasonSignature},
		"RS256 with EC kid":    {p.sign(t, "RS256", "e1", nil, claims(nil)), ReasonSignature},
		"unknown kid":          {p.sign(t, "RS256", "zz", nil, claims(nil)), ReasonKey},
		"no kid":               {p.sign(t, "RS256", "r1", map[string]any{"kid": nil}, claims(nil)), ReasonMalformed},
		"crit":                 {p.sign(t, "RS256", "r1", map[string]any{"crit": []any{"exp"}}, claims(nil)), ReasonMalformed},
		"typ":                  {p.sign(t, "RS256", "r1", map[string]any{"typ": "JOSE+JSON"}, claims(nil)), ReasonMalformed},
		"jku is ignored":       {p.sign(t, "RS256", "zz", map[string]any{"jku": "https://evil/jwks"}, claims(nil)), ReasonKey},
		"expired":              {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"exp": now.Add(-2 * time.Minute).Unix()})), ReasonTime},
		"no exp":               {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"exp": nil})), ReasonTime},
		"no iat":               {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"iat": nil})), ReasonTime},
		"iat in the future":    {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"iat": now.Add(5 * time.Minute).Unix()})), ReasonTime},
		"nbf in the future":    {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"nbf": now.Add(5 * time.Minute).Unix()})), ReasonTime},
		"lifetime":             {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"exp": now.Add(3 * time.Hour).Unix()})), ReasonTime},
		"another audience":     {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"aud": "https://kista.example/other"})), ReasonAudience},
		"no audience":          {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"aud": nil})), ReasonAudience},
		"required claim":       {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"tid": "t2"})), ReasonClaims},
		"required claim gone":  {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"tid": nil})), ReasonClaims},
		"another issuer":       {p.sign(t, "RS256", "r1", nil, claims(map[string]any{"iss": "https://evil.example"})), ReasonIssuer},
		"jwe":                  {"a.b.c.d.e", ReasonMalformed},
		"json serialisation":   {`{"payload":"x"}`, ReasonMalformed},
		"padding":              {p.sign(t, "RS256", "r1", nil, claims(nil)) + "=", ReasonMalformed},
		"oversize":             {strings.Repeat("a", MaxToken+1), ReasonMalformed},
		"duplicate claim keys": {dupToken(t, p), ReasonMalformed},
	} {
		_, _, err := v.Verify(ctx, ta, canonical, tc.tok)
		var f *Failure
		if !errors.As(err, &f) || f.Reason != tc.reason || !errors.Is(err, ErrToken) {
			t.Errorf("%s: %v, want %s", name, err, tc.reason)
		}
	}
	// a GitHub-style token: anyone can get one with the victim's audience; the required claim stops it
	gh := p.sign(t, "RS256", "r1", nil, claims(map[string]any{"tid": nil, "repository_owner": "attacker"}))
	if _, _, err := v.Verify(ctx, ta, canonical, gh); err == nil {
		t.Fatal("a token without the required claim was accepted")
	}
}

func dupToken(t *testing.T, p *idp) string {
	h := enc([]byte(`{"alg":"RS256","kid":"r1"}`))
	c := enc([]byte(`{"iss":"` + p.url + `","sub":"alice","sub":"admin"}`))
	return h + "." + c + "." + enc([]byte("x"))
}

// An unknown-kid flood refreshes the JWKS at most once a minute; a rotated key is picked up.
func TestJWKSRefresh(t *testing.T) {
	p, v, ta, claims := setup(t)
	var now atomic.Int64
	now.Store(time.Unix(1_800_000_000, 0).Unix())
	v.Now = func() time.Time { return time.Unix(now.Load(), 0) }
	if _, _, err := v.Verify(ctx, ta, canonical, p.sign(t, "RS256", "r1", nil, claims(nil))); err != nil {
		t.Fatal(err)
	}
	base := p.gets.Load() // discovery and one JWKS fetch
	for range 50 {
		_, _, _ = v.Verify(ctx, ta, canonical, p.sign(t, "RS256", "unknown", nil, claims(nil)))
	}
	if got := p.gets.Load() - base; got > 1 {
		t.Fatalf("%d JWKS fetches for an unknown-kid flood", got)
	}
	// rotation: a new key appears; after a minute it is fetched
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	p.extra = []map[string]any{{"kty": "RSA", "kid": "r2", "n": enc(rk.N.Bytes()), "e": enc(big.NewInt(65537).Bytes())}}
	old := p.rsa
	p.rsa = rk
	now.Add(61)
	tok := p.sign(t, "RS256", "r2", nil, claims(map[string]any{
		"exp": time.Unix(now.Load(), 0).Add(time.Hour).Unix(), "iat": time.Unix(now.Load(), 0).Unix()}))
	// the first request starts a background refresh and does not wait for it; a later one succeeds
	if !eventually(func() bool { _, _, err := v.Verify(ctx, ta, canonical, tok); return err == nil }) {
		t.Fatal("the rotated key was never picked up")
	}
	p.rsa = old
}

func eventually(f func() bool) bool {
	for range 200 {
		if f() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// cancelFetcher fails every fetch whose context is cancelled, like a client that hangs up.
type cancelFetcher struct{ p *idp }

func (c cancelFetcher) Get(ctx context.Context, url string) ([]byte, http.Header, error) {
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	return c.p.Get(ctx, url)
}

// A key the issuer removed stops working: clients that hang up cannot keep the refresh failing, and
// keys from a failed refresh expire a grace period after their own expiry.
func TestRemovedKeyStopsWorking(t *testing.T) {
	p, v, ta, claims := setup(t)
	var now atomic.Int64
	now.Store(time.Unix(1_800_000_000, 0).Unix())
	v.Now = func() time.Time { return time.Unix(now.Load(), 0) }
	v.Fetch = cancelFetcher{p}
	at := func() map[string]any {
		n := time.Unix(now.Load(), 0)
		return claims(map[string]any{"exp": n.Add(time.Hour).Unix(), "iat": n.Unix()})
	}
	if _, _, err := v.Verify(ctx, ta, canonical, p.sign(t, "RS256", "r1", nil, at())); err != nil {
		t.Fatal(err)
	}
	// the issuer rotates r1 away
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	p.rsa = rk
	p.extra = nil
	// past the JWKS expiry (1h), a request whose client hung up triggers the refresh
	now.Add(3700)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, _, _ = v.Verify(cctx, ta, canonical, "x.y.z")
	_, _, _ = v.Verify(cctx, ta, canonical, p.sign(t, "RS256", "zz", nil, at()))
	old := newIDP(t, p.url)
	old.rsa = nil
	// the refresh ran on its own context: a token signed with the new key (kid r1 again) verifies,
	// so the old r1 key is gone
	if !eventually(func() bool {
		_, _, err := v.Verify(ctx, ta, canonical, p.sign(t, "RS256", "r1", nil, at()))
		return err == nil
	}) {
		t.Fatal("the refresh was cancelled by a client that hung up")
	}
	// an issuer that stays unreachable: stale keys stop verifying after expiry + grace
	v.Fetch = failing{}
	now.Add(int64((defJWKSTTL + staleGrace + time.Minute) / time.Second))
	if _, _, err := v.Verify(ctx, ta, canonical, p.sign(t, "RS256", "r1", nil, at())); err == nil {
		t.Fatal("a key kept verifying long after its JWKS expired")
	}
}

type failing struct{}

func (failing) Get(context.Context, string) ([]byte, http.Header, error) {
	return nil, nil, errors.New("down")
}

// Even a record that (wrongly) lists HS256 never verifies an HMAC with an RSA key, and claim values
// with control or format characters give no principal.
func TestRecordLimits(t *testing.T) {
	p, v, ta, claims := setup(t)
	ta.Issuers[0].Algorithms = []string{"HS256", "none", "RS256"}
	for _, alg := range []string{"HS256", "none"} {
		if _, _, err := v.Verify(ctx, ta, canonical, p.sign(t, alg, "r1", nil, claims(nil))); err == nil {
			t.Fatalf("%s verified", alg)
		}
	}
	pr, _, err := v.Verify(ctx, ta, canonical, p.sign(t, "RS256", "r1", nil, claims(map[string]any{
		"sub": "ali\u0007ce", "realm_access": map[string]any{"roles": []any{"ad\u202emin", "ok"}}})))
	if err != nil {
		t.Fatal(err)
	}
	for k := range pr {
		if k.Kind == "subject" || k.Value == "ad\u202emin" {
			t.Fatalf("an unclean claim value became a principal: %v", k)
		}
	}
	if !pr[Key{"iss-1", "role", "ok"}] {
		t.Fatalf("principals %v", pr)
	}
}

func TestJWKRefusals(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	for name, r := range map[string]rawJWK{
		"no kid":    {Kty: "RSA", N: enc(small.N.Bytes()), E: "AQAB"},
		"1024-bit":  {Kty: "RSA", Kid: "k", N: enc(small.N.Bytes()), E: "AQAB"},
		"private":   {Kty: "OKP", Kid: "k", Crv: "Ed25519", X: enc(make([]byte, 32)), D: "x"},
		"enc use":   {Kty: "OKP", Kid: "k", Crv: "Ed25519", X: enc(make([]byte, 32)), Use: "enc"},
		"bad curve": {Kty: "EC", Kid: "k", Crv: "P-521"},
		"off curve": {Kty: "EC", Kid: "k", Crv: "P-256", X: enc(make([]byte, 32)), Y: enc(make([]byte, 32))},
		"symmetric": {Kty: "oct", Kid: "k"},
		"short ed":  {Kty: "OKP", Kid: "k", Crv: "Ed25519", X: enc(make([]byte, 31))},
	} {
		if _, err := parseJWK(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAllows(t *testing.T) {
	p := Principals{{"i1", "subject", "alice"}: true, {"i1", "issuer", ""}: true}
	g := func(kind, value, ch, ext string, verbs ...string) store.Grant {
		return store.Grant{IssuerID: "i1", Kind: kind, Value: value, ChannelID: ch, Extension: ext, Verbs: verbs}
	}
	for name, tc := range map[string]struct {
		grants []store.Grant
		want   bool
	}{
		"tenant":               {[]store.Grant{g("subject", "alice", "", "", "install")}, true},
		"channel":              {[]store.Grant{g("subject", "alice", "c1", "", "install")}, true},
		"other channel":        {[]store.Grant{g("subject", "alice", "c2", "", "install")}, false},
		"extension":            {[]store.Grant{g("subject", "alice", "", "tresor", "install")}, true},
		"other extension":      {[]store.Grant{g("subject", "alice", "", "acl", "install")}, false},
		"extension in channel": {[]store.Grant{g("subject", "alice", "c1", "tresor", "install")}, true},
		"admin":                {[]store.Grant{g("subject", "alice", "", "", "admin")}, true},
		"issuer-wide":          {[]store.Grant{g("issuer", "", "", "", "install")}, true},
		"another subject":      {[]store.Grant{g("subject", "bob", "", "", "install")}, false},
		"another issuer":       {[]store.Grant{{IssuerID: "i2", Kind: "subject", Value: "alice", Verbs: []string{"install"}}}, false},
		"none":                 {nil, false},
	} {
		if got := Allows(p, tc.grants, "c1", "tresor", "install"); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
