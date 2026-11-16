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
	// unverified: nothing of it is trusted (ErrStatement; a feed holds the body)
	member := func(m string) ([]byte, []byte) {
		return sign2(t, key, strings.Replace(string(b), `{"audience"`, `{`+m+`,"audience"`, 1))
	}
	bad := map[string]func() ([]byte, []byte){
		"altered":      func() ([]byte, []byte) { return []byte(strings.Replace(string(b), `"R"`, `"S"`, 1)), sig },
		"no signature": func() ([]byte, []byte) { return b, sig[:0] },
		"unknown":      func() ([]byte, []byte) { s := base; s.Key = "sha256:" + strings.Repeat("0", 64); return sign(s) },
		"not canonical": func() ([]byte, []byte) {
			x, _ := json.MarshalIndent(base, "", " ")
			return sign2(t, key, string(x))
		},
		// spec 0046, the member-name rule: no two names may mean one field to Go's decoder, which
		// folds case (Unicode's too) and keeps the last duplicate
		"Kind beside kind": func() ([]byte, []byte) { return member(`"Kind":"notes"`) },
		"the Kelvin sign": func() ([]byte, []byte) {
			return sign2(t, key, strings.Replace(string(b), `"kind"`, "\"\u212aind\"", 1))
		},
		"the long s": func() ([]byte, []byte) { return sign2(t, key, strings.Replace(string(b), `"seq"`, "\"\u017feq\"", 1)) },
		"a long s beside": func() ([]byte, []byte) {
			return sign2(t, key, strings.Replace(string(b), `"seq":7`, "\"seq\":7,\"\u017feq\":9", 1))
		},
		"a duplicate": func() ([]byte, []byte) {
			return sign2(t, key, strings.Replace(string(b), `"seq":7`, `"seq":7,"seq":8`, 1))
		},
		"a nested duplicate": func() ([]byte, []byte) {
			return sign2(t, key, strings.Replace(string(b), `"seq":7`, `"seq":7,"since":{"a":1,"a":2}`, 1))
		},
		"a nested name": func() ([]byte, []byte) {
			return sign2(t, key, strings.Replace(string(b), `"seq":7`, `"seq":7,"since":{"A":1}`, 1))
		},
		"a name too long": func() ([]byte, []byte) { return member(`"a` + strings.Repeat("a", 64) + `":1`) },
		// integers only, in any member, known or not
		"a fraction in an unknown member":  func() ([]byte, []byte) { return member(`"a":1.5`) },
		"an exponent in an unknown member": func() ([]byte, []byte) { return member(`"a":[{"b":1e2}]`) },
		"a fraction in a known member": func() ([]byte, []byte) {
			return sign2(t, key, strings.Replace(string(b), `"size":10`, `"size":10.0`, 1))
		},
		"not an object": func() ([]byte, []byte) { return sign2(t, key, `[1]`) },
		"null":          func() ([]byte, []byte) { return sign2(t, key, `null`) },
	}
	for name, f := range bad {
		x, s := f()
		if st, err := Verify(x, s, pinned); !errors.Is(err, ErrStatement) || errors.Is(err, ErrMalformed) || st.Issuer != "" {
			t.Errorf("%s: %v", name, err)
		}
	}

	// spec 0046: an unknown member is signed like the others and ignored, at any depth
	x, xs := member(`"a":{"b":[1,"c",true,null,{"d":-2}]},"a2":"x"`)
	if got, err := Verify(x, xs, pinned); err != nil || got.Kind != "review" || got.Seq != 7 || got.Major() != 1 || got.V != 0 {
		t.Errorf("an extra member: %+v %v", got, err)
	}
	// an audience that is not readers and any printable origin are read (the feed stores for administrators)
	for _, mod := range []func(*Statement){
		func(s *Statement) { s.Audience = "partners" },
		func(s *Statement) { s.Audience = "" },
		func(s *Statement) { s.Origin = "distributor:acme" },
		func(s *Statement) { s.Kind, s.MediaType = "report", "application/vnd.example.report+json" }, // a kind of a later release
		func(s *Statement) { s.V = 1 },
	} {
		s := base
		mod(&s)
		if x, sg := sign(s); func() bool { _, err := Verify(x, sg, pinned); return err != nil }() {
			t.Errorf("refused: %s", x)
		}
	}

	// authentic, with a frozen or known member of the wrong type or out of its bound: refused for
	// good (ErrMalformed), with what files it when that can be read
	raw := func(old, new string) func() ([]byte, []byte) {
		return func() ([]byte, []byte) {
			if !strings.Contains(string(b), old) {
				t.Fatalf("the statement has no %s", old)
			}
			return sign2(t, key, strings.Replace(string(b), old, new, 1))
		}
	}
	malformed := map[string]struct {
		f     func() ([]byte, []byte)
		filed bool
	}{
		"withdrawal with content": {func() ([]byte, []byte) { s := base; s.Withdrawn = true; return sign(s) }, true},
		"bad origin":              {func() ([]byte, []byte) { s := base; s.Origin = "a\u200bb"; return sign(s) }, true},
		"a long origin":           {func() ([]byte, []byte) { s := base; s.Origin = strings.Repeat("o", 81); return sign(s) }, true},
		"no origin":               {func() ([]byte, []byte) { s := base; s.Origin = ""; return sign(s) }, true},
		"bad time":                {func() ([]byte, []byte) { s := base; s.IssuedAt = "yesterday"; return sign(s) }, true},
		"a kind's grammar":        {func() ([]byte, []byte) { s := base; s.Kind = "Review"; return sign(s) }, true},
		"v 0":                     {raw(`"title":"R"`, `"title":"R","v":0`), true},
		"v a string":              {raw(`"title":"R"`, `"title":"R","v":"2"`), true},
		"size a string":           {raw(`"size":10`, `"size":"10"`), true},
		"title a number":          {raw(`"title":"R"`, `"title":7`), true},
		"audience a list":         {raw(`"audience":"readers"`, `"audience":["readers"]`), true},
		"withdrawn a string":      {raw(`"title":"R"`, `"title":"R","withdrawn":"yes"`), true},
		"kind null":               {raw(`"kind":"review"`, `"kind":null`), true},
		"bad body":                {func() ([]byte, []byte) { s := base; s.BodyHash = "x"; return sign(s) }, false},
		"zero seq":                {func() ([]byte, []byte) { s := base; s.Seq = 0; return sign(s) }, false},
		"seq a string":            {raw(`"seq":7`, `"seq":"7"`), false},
		"issuer a number":         {raw(`"issuer":"https://src.example/api/v1/tenants/x"`, `"issuer":1`), false},
	}
	for name, c := range malformed {
		x, s := c.f()
		st, err := Verify(x, s, pinned)
		if !errors.Is(err, ErrMalformed) || errors.Is(err, ErrStatement) {
			t.Errorf("%s: %v", name, err)
		}
		if filed := st.Issuer == base.Issuer && st.BodyHash == base.BodyHash && st.Seq == 7 && st.IssuerID == "d1"; filed != c.filed ||
			st.Kind != "" || st.Title != "" {
			t.Errorf("%s: filed as %+v", name, st)
		}
	}

	// a major above 1: verified, and only its frozen members are read, whatever the others became
	v2, v2s := sign2(t, key, strings.Replace(strings.Replace(string(b), `"size":10`, `"size":{"bytes":10}`, 1), `"title":"R"`, `"title":"R","v":2`, 1))
	got, err = Verify(v2, v2s, pinned)
	if err != nil || got.Major() != 2 || got.Issuer != base.Issuer || got.IssuerID != "d1" || got.Seq != 7 || got.BodyHash != base.BodyHash ||
		got.Audience != "readers" || got.Withdrawn || got.Kind != "" || got.Title != "" || got.Size != 0 || got.ContentHash != "" {
		t.Errorf("v 2: %+v %v", got, err)
	}
	w2, w2s := sign2(t, key, strings.Replace(string(v2), `"v":2`, `"v":2,"withdrawn":true`, 1))
	if got, err := Verify(w2, w2s, pinned); err != nil || !got.Withdrawn || got.Major() != 2 {
		t.Errorf("v 2, withdrawn: %+v %v", got, err)
	}
	// its frozen members are bounded all the same
	bad2, bad2s := sign2(t, key, strings.Replace(string(v2), `"seq":7`, `"seq":0`, 1))
	if _, err := Verify(bad2, bad2s, pinned); !errors.Is(err, ErrMalformed) {
		t.Errorf("v 2 with seq 0: %v", err)
	}

	w := Statement{Issuer: base.Issuer, Key: fp, IssuerID: "d1", Seq: 8, BodyHash: base.BodyHash, IssuedAt: base.IssuedAt, Withdrawn: true}
	if x, s := sign(w); func() bool { _, err := Verify(x, s, pinned); return err != nil }() {
		t.Error("a withdrawal refused")
	}
}

// The member-name rule on its own: the documents a signed or bundled format may be.
func TestMemberNames(t *testing.T) {
	for doc, ok := range map[string]bool{
		`{"a":1,"b_2":{"c":[{"d":1},{"d":2}]}}`: true,
		`{}`:                                    true,
		`[{"a":1},{"a":1}]`:                     true, // the same name in two objects
		`{"a":{"a":1}}`:                         true,
		`{"A":1}`:                               false,
		`{"a-b":1}`:                             false,
		`{"":1}`:                                false,
		`{"a":1,"a":2}`:                         false,
		`{"a":[{"b":1,"b":2}]}`:                 false,
		"{\"\u212a\":1}":                        false,
		`{"\u0061":1,"a":2}`:                    false, // the same name, one escaped
		`{"a":`:                                 false,
	} {
		if err := MemberNames([]byte(doc)); (err == nil) != ok {
			t.Errorf("%s: %v", doc, err)
		}
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

// The issuer is the form a subscriber normalises its feed URL to.
func TestIssuer(t *testing.T) {
	for in, want := range map[string]string{
		"https://Kista.Example":      "https://kista.example/api/v1/tenants/acme",
		"https://kista.example:443/": "https://kista.example/api/v1/tenants/acme",
		"https://kista.example:8443": "https://kista.example:8443/api/v1/tenants/acme",
	} {
		if got := Issuer(in, "acme"); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
