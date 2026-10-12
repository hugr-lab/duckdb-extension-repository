package attachment

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

// RFC 8785's rules on what statements carry: members sorted by UTF-16 code units, the shortest
// string escapes, integers only.
func TestCanonicalize(t *testing.T) {
	for in, want := range map[string]string{
		`{"b":1, "a":[true,null,"x"]}`:             `{"a":[true,null,"x"],"b":1}`,
		`{"€":1,"😀":2,"a":3}`:                      `{"a":3,"€":1,"😀":2}`, // U+20AC < U+D83D (a surrogate) in UTF-16
		`{"s":"\u0001\n\"\\ </>&é"}`:               `{"s":"\u0001\n\"\\` + " " + `</>&é"}`,
		`{"n":-9007199254740992,"m":0}`:            `{"m":0,"n":-9007199254740992}`,
		`{"nested":{"z":{"y":1,"x":2},"a":"b"}}  `: `{"nested":{"a":"b","z":{"x":2,"y":1}}}`,
	} {
		got, err := Canonicalize([]byte(in))
		if err != nil || string(got) != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	for _, in := range []string{`{"n":1.5}`, `{"n":1e3}`, `{"n":9007199254740993}`, `{} {}`, `{"a":`} {
		if _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

type testSigner struct{ k *rsa.PrivateKey }

func (s testSigner) Sign(_ context.Context, h extfile.BodyHash) ([]byte, error) {
	return rsa.SignPKCS1v15(rand.Reader, s.k, crypto.SHA256, h[:])
}
func (s testSigner) Public() *rsa.PublicKey { return &s.k.PublicKey }
func (s testSigner) ID() string             { return "test" }

// A statement verifies only as signed, by a pinned key it names, in its canonical form, with its
// fields bounded; the signature is domain-separated from a body hash's.
func TestVerify(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	fp := extfile.Fingerprint(&key.PublicKey)
	pinned := map[string]*rsa.PublicKey{fp: &key.PublicKey}
	base := Statement{Issuer: "https://src.example/api/v1/tenants/x", Key: fp, IssuerID: "d1", Seq: 7, BodyHash: strings.Repeat("a", 64),
		Kind: "review", MediaType: Review, ContentHash: strings.Repeat("b", 64), Size: 10, Title: "R",
		Summary: json.RawMessage(`{"verdict":"approved"}`), IssuedAt: "2026-10-03T12:00:00Z", Origin: "tenant", Audience: "readers"}
	sign := func(s Statement) ([]byte, []byte) {
		b, err := s.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		sig, err := Sign(context.Background(), testSigner{key}, b)
		if err != nil {
			t.Fatal(err)
		}
		return b, sig
	}
	b, sig := sign(base)
	got, err := Verify(b, sig, pinned)
	if err != nil || got.Title != "R" || got.Seq != 7 {
		t.Fatalf("verify: %+v %v", got, err)
	}
	// the signature is not a body hash's: the same key's signature over the statement's SHA-256 alone fails
	if plain, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sha(b)); func() bool { _, err := Verify(b, plain, pinned); return err == nil }() {
		t.Error("a signature without the domain separator verified")
	}
	bad := map[string]func() ([]byte, []byte){
		"altered":      func() ([]byte, []byte) { return []byte(strings.Replace(string(b), `"R"`, `"S"`, 1)), sig },
		"no signature": func() ([]byte, []byte) { return b, sig[:0] },
		"unknown":      func() ([]byte, []byte) { s := base; s.Key = "sha256:" + strings.Repeat("0", 64); return sign(s) },
		"extra field": func() ([]byte, []byte) {
			x := strings.Replace(string(b), `{"audience"`, `{"a":1,"audience"`, 1)
			return sign2(t, key, x)
		},
		"not canonical": func() ([]byte, []byte) {
			x, _ := json.MarshalIndent(base, "", " ")
			return sign2(t, key, string(x))
		},
		"withdrawal with content": func() ([]byte, []byte) { s := base; s.Withdrawn = true; return sign(s) },
		"bad audience":            func() ([]byte, []byte) { s := base; s.Audience = "everyone"; return sign(s) },
		"bad origin":              func() ([]byte, []byte) { s := base; s.Origin = "publisher:"; return sign(s) },
		"bad body":                func() ([]byte, []byte) { s := base; s.BodyHash = "x"; return sign(s) },
		"zero seq":                func() ([]byte, []byte) { s := base; s.Seq = 0; return sign(s) },
		"bad time":                func() ([]byte, []byte) { s := base; s.IssuedAt = "yesterday"; return sign(s) },
	}
	for name, f := range bad {
		x, s := f()
		if _, err := Verify(x, s, pinned); !errors.Is(err, ErrStatement) {
			t.Errorf("%s: %v", name, err)
		}
	}
	w := Statement{Issuer: base.Issuer, Key: fp, IssuerID: "d1", Seq: 8, BodyHash: base.BodyHash, IssuedAt: base.IssuedAt, Withdrawn: true}
	if x, s := sign(w); func() bool { _, err := Verify(x, s, pinned); return err != nil }() {
		t.Error("a withdrawal refused")
	}
}

func sha(b []byte) []byte {
	h := crypto.SHA256.New()
	h.Write(b)
	return h.Sum(nil)
}

func sign2(t *testing.T, key *rsa.PrivateKey, raw string) ([]byte, []byte) {
	t.Helper()
	sig, err := Sign(context.Background(), testSigner{key}, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(raw), sig
}
