package api_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/symbols/symtest"
)

// keyed is built with a key block after its code (spec 0021).
func keyed(t *testing.T, version string, keyBlock []byte, exports ...string) []byte {
	t.Helper()
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0",
		ExtensionVersion: version, ABI: extfile.ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	b := append(symtest.ELF(exports...), keyBlock...)
	b = append(b, extfile.MetadataPrefix...)
	b = append(b, block[:]...)
	return append(b, make([]byte, extfile.SignatureSize)...)
}

// Spec 0021: a marked publisher's publications record their builds' licence keys; the mark is the
// server administrator's, and so are the marked publisher's credentials and grants.
func TestLicenseKeySources(t *testing.T) {
	m := newMgmt(t)
	sa, ta := m.toks["server admin"], m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"
	pemKey, err := extfile.MarshalPublicKeyPEM(&m.licKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	block, err := extfile.EncodeLicenseKeys(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	kid := vectors.Kid(&m.licKey.PublicKey)
	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	publisher := func(name string) string {
		t.Helper()
		if r := m.call(t, "POST", T+"/publishers", ta, `{"name":"`+name+`"}`); r.status != 201 {
			t.Fatalf("publisher %s: %d %s", name, r.status, r.body)
		}
		if r := m.call(t, "POST", T+"/grants", ta, `{"principal":"publisher:`+name+`","verbs":["publish"],"channel":"prod"}`); r.status != 201 {
			t.Fatalf("grant: %d %s", r.status, r.body)
		}
		r := m.call(t, "POST", T+"/publishers/"+name+"/keys", ta, `{"expires_at":"`+exp+`"}`)
		if r.status != 201 {
			t.Fatalf("key: %d %s", r.status, r.body)
		}
		return r.json(t)["key"].(string)
	}
	keys := func() string {
		t.Helper()
		r := m.call(t, "GET", T+"/license-keys", ta, "")
		if r.status != 200 {
			t.Fatalf("license keys: %d %s", r.status, r.body)
		}
		return fmt.Sprint(r.json(t)["names"])
	}
	const P = T + "/channels/prod/extensions/"
	issuer, other := publisher("issuer-ci"), publisher("other-ci")

	// an unmarked publisher's publication records nothing
	if r := m.upload(t, P+"newext/releases?version=1.0&platform=linux_amd64", other, keyed(t, "1.0", block, "newext_duckdb_cpp_init")); r.status != 201 {
		t.Fatalf("an unmarked publication: %d %s", r.status, r.body)
	}
	if got := keys(); got != "[]" {
		t.Fatalf("keys from an unmarked publisher: %s", got)
	}

	// the mark: a server administrator's
	if r := m.call(t, "POST", T+"/publishers/issuer-ci/license-keys", ta, ""); r.status != 404 {
		t.Fatalf("a tenant administrator marks: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/publishers/issuer-ci/license-keys", sa, ""); r.status != 200 || r.json(t)["license_keys"] != true {
		t.Fatalf("mark: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/publishers/issuer-ci", ta, ""); r.json(t)["license_keys"] != true {
		t.Fatalf("the mark is not shown: %s", r.body)
	}

	// marked: its credentials and grants are the server administrator's
	var grantID string
	for _, g := range m.call(t, "GET", T+"/grants", ta, "").json(t)["grants"].([]any) {
		if g := g.(map[string]any); g["principal"] == "publisher:issuer-ci" {
			grantID = g["id"].(string)
		}
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", T + "/publishers/issuer-ci/keys", `{"expires_at":"` + exp + `"}`},
		{"POST", T + "/grants", `{"principal":"publisher:issuer-ci","verbs":["publish"],"channel":"staging"}`},
		{"DELETE", T + "/grants/" + grantID, ""},
		{"POST", T + "/publishers/issuer-ci/github", `{"owner_id":"1","repository_id":"2","workflow":"o/r/.github/workflows/w.yml"}`},
	} {
		if r := m.call(t, c.method, c.path, ta, c.body); r.status != 404 {
			t.Errorf("a tenant administrator's %s %s on a marked publisher: %d %s", c.method, c.path, r.status, r.body)
		}
	}
	if r := m.call(t, "POST", T+"/grants", sa, `{"principal":"publisher:issuer-ci","verbs":["publish"],"channel":"staging"}`); r.status != 201 {
		t.Fatalf("a server administrator's grant to a marked publisher: %d %s", r.status, r.body)
	}

	// a malformed block is refused; a sound one records the key
	twoBlocks := append(append([]byte{}, block...), append([]byte("x"), block[:len(block)-12]...)...)
	if r := m.upload(t, P+"newext/releases?version=1.1&platform=linux_amd64", issuer, keyed(t, "1.1", twoBlocks, "newext_duckdb_cpp_init")); r.status != 400 ||
		!strings.Contains(string(r.body), "license_keys") {
		t.Fatalf("a malformed block: %d %s", r.status, r.body)
	}
	// the body the unmarked publisher released, published again by the marked one
	if r := m.upload(t, P+"newext/releases?version=1.0&platform=linux_amd64", issuer, keyed(t, "1.0", block, "newext_duckdb_cpp_init")); r.status != 200 {
		t.Fatalf("publish again: %d %s", r.status, r.body)
	}
	if got := keys(); !strings.Contains(got, "newext") || !strings.Contains(got, kid) || !strings.Contains(got, "issuer-ci") {
		t.Fatalf("keys after the marked publication: %s", got)
	}
	if r := m.call(t, "POST", T+"/licenses", ta, m.licence("newext", "pro")); r.status != 201 {
		t.Fatalf("an import against the recorded key: %d %s", r.status, r.body)
	}

	// unmarking keeps the keys; removing the publisher drops them
	if r := m.call(t, "DELETE", T+"/publishers/issuer-ci/license-keys", sa, ""); r.status != 200 || r.json(t)["license_keys"] != false {
		t.Fatalf("unmark: %d %s", r.status, r.body)
	}
	if got := keys(); !strings.Contains(got, kid) {
		t.Fatalf("keys after unmarking: %s", got)
	}
	if r := m.call(t, "POST", T+"/publishers/issuer-ci/license-keys", sa, ""); r.status != 200 {
		t.Fatalf("mark again: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/publishers/issuer-ci", ta, ""); r.status != 204 {
		t.Fatalf("a tenant administrator removes a marked publisher: %d %s", r.status, r.body)
	}
	if got := keys(); got != "[]" {
		t.Fatalf("keys after removing the publisher: %s", got)
	}
}
