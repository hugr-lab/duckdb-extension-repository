package license_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
)

func loadVectors(t *testing.T) vectors.File {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f vectors.File
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestVectors checks the committed vectors against the verifier: what the C++ SDK is tested
// against is what kista does.
func TestVectors(t *testing.T) {
	f := loadVectors(t)
	keys, err := license.ParseKeys(f.Keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 100 {
		t.Fatalf("only %d cases", len(f.Cases))
	}
	seen := map[license.Status]bool{}
	for _, c := range f.Cases {
		claims, st, err := license.Verify(c.Token, keys, c.Ext, c.Version, time.Unix(c.Now, 0))
		seen[st] = true
		if st != c.Status {
			t.Errorf("%s: status %s, want %s (%v)", c.Name, st, c.Status, err)
			continue
		}
		var le *license.Error
		if rule := ""; errors.As(err, &le) {
			rule = le.Rule
			if rule != c.Rule {
				t.Errorf("%s: refused for %s, want %s", c.Name, rule, c.Rule)
			}
		} else if err != nil || c.Rule != "" {
			t.Errorf("%s: error %v, want rule %q", c.Name, err, c.Rule)
		}
		if (st == license.Invalid) != (err != nil) {
			t.Errorf("%s: status %s with error %v", c.Name, st, err)
		}
		if !reflect.DeepEqual(claims, c.Claims) {
			t.Errorf("%s: claims %+v, want %+v", c.Name, claims, c.Claims)
		}
	}
	for _, s := range []license.Status{license.Invalid, license.NotCovered, license.NotYetValid, license.Valid, license.Grace, license.Expired} {
		if !seen[s] {
			t.Errorf("no vector has status %s", s)
		}
	}
}

// TestVectorsReproducible fails when vectors.json is not what the generator writes: regenerate it
// with go run ./internal/license/vectorgen.
func TestVectorsReproducible(t *testing.T) {
	got, err := vectors.Generate("testdata")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("testdata/vectors.json is stale: run go run ./internal/license/vectorgen")
	}
}

func publicPEM(t *testing.T, name string) string {
	t.Helper()
	k, err := vectors.LoadKey("testdata", name)
	if err != nil {
		t.Fatal(err)
	}
	p, err := extfile.MarshalPublicKeyPEM(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseKeys(t *testing.T) {
	a, b := publicPEM(t, vectors.TrustedKey), publicPEM(t, vectors.UntrustedKey)
	ks, err := license.ParseKeys([]string{a, b, a})
	if err != nil {
		t.Fatal(err)
	}
	if len(ks) != 2 {
		t.Fatalf("%d keys, want 2", len(ks))
	}
	for _, n := range []string{vectors.Key3072, vectors.Key1024} {
		if _, err := license.ParseKeys([]string{a, publicPEM(t, n)}); err == nil || !strings.Contains(err.Error(), "key 2") {
			t.Errorf("%s: %v, want a refusal of key 2", n, err)
		}
	}
	if _, err := license.ParseKeys([]string{"not a key"}); err == nil {
		t.Error("a non-key was accepted")
	}
}

func TestStatusAt(t *testing.T) {
	c := &license.Claims{IssuedAt: 1000, ExpiresAt: 2000, Grace: 100}
	for _, tc := range []struct {
		now  int64
		want license.Status
	}{{699, license.NotYetValid}, {700, license.Valid}, {1999, license.Valid}, {2000, license.Grace}, {2099, license.Grace}, {2100, license.Expired}} {
		if got := c.StatusAt(time.Unix(tc.now, 0)); got != tc.want {
			t.Errorf("at %d: %s, want %s", tc.now, got, tc.want)
		}
	}
}

// TestLimitsJSON checks that limits come out as the scalars they were.
func TestLimitsJSON(t *testing.T) {
	f := loadVectors(t)
	keys, _ := license.ParseKeys(f.Keys)
	c, st, err := license.Verify(f.Cases[0].Token, keys, "myext", "", time.Unix(vectors.T0, 0))
	if err != nil || st != license.Valid {
		t.Fatalf("%s %v", st, err)
	}
	b, err := json.Marshal(c.Limits)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"max_attached_sources","value":5},{"name":"export","value":true},{"name":"tier","value":"gold"}]`
	if string(b) != want {
		t.Fatalf("limits %s, want %s", b, want)
	}
}

// FuzzParse checks that no input panics the parser, and that whatever it accepts is a licence by
// the rules: three parts, valid JSON both ways.
func FuzzParse(f *testing.F) {
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		f.Fatal(err)
	}
	var vf vectors.File
	if err := json.Unmarshal(b, &vf); err != nil {
		f.Fatal(err)
	}
	for _, c := range vf.Cases {
		f.Add(c.Token)
	}
	f.Fuzz(func(t *testing.T, tok string) {
		p, err := license.Parse(tok)
		if err != nil {
			return
		}
		if strings.Count(tok, ".") != 2 || p.Key == "" || p.Extension == "" || p.ExpiresAt <= p.IssuedAt {
			t.Fatalf("accepted %q as %+v", tok, p.Claims)
		}
	})
}

// TestSentinels checks that callers can tell an untrusted key and a bad signature from a malformed
// token, and that a stored token's claims read back.
func TestSentinels(t *testing.T) {
	f := loadVectors(t)
	keys, _ := license.ParseKeys(f.Keys)
	by := map[string]vectors.Case{}
	for _, c := range f.Cases {
		by[c.Name] = c
	}
	for name, want := range map[string]error{
		"signed by test-only-untrusted-2048":          license.ErrUntrustedKey,
		"trusted kid, signed by another 2048-bit key": license.ErrSignature,
		"another extension":                           license.ErrWrongExtension,
	} {
		c := by[name]
		_, _, err := license.Verify(c.Token, keys, c.Ext, c.Version, time.Unix(c.Now, 0))
		if !errors.Is(err, want) {
			t.Errorf("%s: %v is not %v", name, err, want)
		}
		if errors.Is(err, license.ErrSignature) != (want == license.ErrSignature) {
			t.Errorf("%s: %v matches ErrSignature", name, err)
		}
	}
	tok, err := license.Parse(by["valid"].Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.Verify(keys); err != nil {
		t.Fatal(err)
	}
	if st, err := tok.Status("myext", "", time.Unix(vectors.T0, 0)); st != license.Valid || err != nil {
		t.Fatalf("%s %v", st, err)
	}
	if n, ok := tok.Limits[0].Value.Int(); !ok || n != 5 {
		t.Fatalf("limit %v %v", n, ok)
	}
}
