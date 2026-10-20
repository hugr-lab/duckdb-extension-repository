package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0023: a tenant administrator registers a publisher's public SSH key; requests signed with
// it publish and promote, read what the publisher may promote, and do nothing else.
func TestSignedPublishing(t *testing.T) {
	m := newMgmt(t)
	ta := m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"

	// the publisher, its grants (by the tenant administrator, over the API)
	if r := m.call(t, "POST", T+"/publishers", ta, `{"name":"acl-ci"}`); r.status != 201 || r.header.Get("Location") != T+"/publishers/acl-ci" {
		t.Fatalf("add publisher: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/publishers", ta, `{"name":"acl-ci"}`); r.status != 409 {
		t.Errorf("the same publisher: %d", r.status)
	}
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"publish on staging":   {`{"principal":"publisher:acl-ci","verbs":["publish"],"channel":"staging","extension":"newext"}`, 201},
		"promote to prod":      {`{"principal":"publisher:acl-ci","verbs":["promote"],"channel":"prod","extension":"newext"}`, 201},
		"install is refused":   {`{"principal":"publisher:acl-ci","verbs":["install"]}`, 400},
		"admin is refused":     {`{"principal":"publisher:acl-ci","verbs":["admin"]}`, 400},
		"an unknown publisher": {`{"principal":"publisher:nope","verbs":["publish"]}`, 400},
	} {
		if r := m.call(t, "POST", T+"/grants", ta, tc.body); r.status != tc.want {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// keys: an OpenSSH line of an Ed25519 or ECDSA P-256 key, unique in the tenant, expiring within a year
	k := newSigner(t)
	for name, body := range map[string]string{
		"not a key":   `{"public_key":"hello"}`,
		"rsa":         `{"public_key":"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7 x"}`,
		"past":        `{"public_key":"` + k.authKey + `","expires_at":"2001-01-01T00:00:00Z"}`,
		"over a year": `{"public_key":"` + k.authKey + `","expires_at":"` + time.Now().Add(400*24*time.Hour).UTC().Format(time.RFC3339) + `"}`,
		"not a time":  `{"public_key":"` + k.authKey + `","expires_at":"soon"}`,
	} {
		if r := m.call(t, "POST", T+"/publishers/acl-ci/keys", ta, body); r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	r := m.call(t, "POST", T+"/publishers/acl-ci/keys", ta, `{"public_key":"`+k.authKey+`"}`)
	if r.status != 201 || r.json(t)["fingerprint"] != k.fp || r.json(t)["title"] != "ci" || r.json(t)["type"] != "ssh-ed25519" ||
		!strings.HasPrefix(r.header.Get("Location"), T+"/publishers/acl-ci/keys/") {
		t.Fatalf("add key: %d %s", r.status, r.body)
	}
	k.id = r.json(t)["id"].(string)
	ci := m.hold(k)
	m.call(t, "POST", T+"/publishers", ta, `{"name":"zz-ci"}`)
	if r := m.call(t, "POST", T+"/publishers/zz-ci/keys", ta, `{"public_key":"`+k.authKey+`"}`); r.status != 409 {
		t.Errorf("the same key on another publisher: %d %s", r.status, r.body)
	}
	if g := m.call(t, "GET", T+"/publishers/acl-ci/keys/"+k.id, ta, ""); g.status != 200 || g.json(t)["public_key"] == nil {
		t.Errorf("get key: %d %s", g.status, g.body)
	}
	if g := m.call(t, "GET", T+"/publishers/acl-ci", ta, "").json(t); len(g["keys"].([]any)) != 1 {
		t.Errorf("the publisher's keys: %v", g)
	}
	// an issuer record cannot take GitHub Actions' URL
	if r := m.call(t, "POST", T+"/issuers", ta, `{"name":"gh","url":"`+auth.GitHubActionsIssuer+`","required_claims":{"repository_owner_id":"10"}}`); r.status != 400 ||
		!strings.Contains(string(r.body), "CI token issuer") {
		t.Errorf("an issuer record at GitHub's URL: %d %s", r.status, r.body)
	}

	S := T + "/channels/staging/extensions/newext/releases"
	file := built(t, "1.0", "newext_duckdb_cpp_init")
	r = m.upload(t, S+"?version=1.0&platform=linux_amd64", ci, file)
	if r.status != 201 {
		t.Fatalf("publish by a signed request: %d %s", r.status, r.body)
	}
	rel := r.json(t)
	prov, _ := rel["provenance"].(map[string]any)
	if prov["provider"] != "key" || prov["actor"] != nil || len(prov["credentials"].([]any)) != 1 || prov["credentials"].([]any)[0] != "key:"+k.fp {
		t.Errorf("the publication's provenance (to the publisher: no actor): %v", prov)
	}
	// the signed request is kept, and verifies with the key (administrators see it, not publishers)
	if rel["publisher_statement"] != nil {
		t.Errorf("the publisher sees its signed request: %v", rel)
	}
	adm := m.call(t, "GET", S+"/"+rel["id"].(string), ta, "").json(t)
	st, _ := adm["publisher_statement"].(string)
	sig, _ := base64.StdEncoding.DecodeString(adm["publisher_signature"].(string))
	if !strings.Contains(st, "path "+S+"?version=1.0&platform=linux_amd64\n") {
		t.Errorf("the statement kept: %q", st)
	}
	if sr, err := auth.ParseSignedRequest(base64.StdEncoding.EncodeToString([]byte(st)), base64.StdEncoding.EncodeToString(sig)); err != nil ||
		sr.Verify(k.s.PublicKey().Marshal()) != nil {
		t.Errorf("the signature kept: %v", err)
	}
	// a tenant administrator sees who published it
	if g := m.call(t, "GET", S+"/"+rel["id"].(string), ta, "").json(t); g["created_by"] != "publisher:acme/acl-ci" {
		t.Errorf("created_by for the administrator: %v", g["created_by"])
	}
	// the same request again (a replay, or a retry) is the existing release
	if r := m.upload(t, S+"?version=1.0&platform=linux_amd64", ci, file); r.status != 200 {
		t.Errorf("the same publication again: %d %s", r.status, r.body)
	}
	// it reads what it may promote, and promotes
	if got := m.call(t, "GET", S, ci, "").json(t)["releases"].([]any); len(got) != 1 {
		t.Errorf("the publisher's release list: %v", got)
	}
	r = m.call(t, "POST", T+"/channels/prod/extensions/newext/releases/promote", ci, `{"from_channel":"staging","version":"1.0"}`)
	if r.status != 201 {
		t.Fatalf("promote by a signed request: %d %s", r.status, r.body)
	}
	pr := r.json(t)["releases"].([]any)[0].(map[string]any)
	pa := m.call(t, "GET", T+"/channels/prod/extensions/newext/releases/"+pr["id"].(string), ta, "").json(t)
	if pp, _ := pr["provenance"].(map[string]any); pp["from_channel"] != "staging" || pp["provider"] != "key" ||
		!strings.Contains(fmt.Sprint(pa["publisher_statement"]), "/releases/promote\n") {
		t.Errorf("a promotion's provenance and statement: %v", pr)
	}
	w := m.call(t, "GET", T+"/whoami", ci, "").json(t)
	if pubs, _ := w["publishers"].([]any); w["provider"] != "key" || len(pubs) != 1 || len(w["grants"].([]any)) != 2 ||
		pubs[0].(map[string]any)["credential"] != "key:"+k.fp {
		t.Errorf("whoami: %v", w)
	}
	// nothing else: management is not for it, nor a route without a tenant; the index is the public view
	for _, path := range []string{T + "/grants", T + "/publishers", "/api/v1/tenants"} {
		if r := m.call(t, "GET", path, ci, ""); r.status != 401 {
			t.Errorf("%s: %d", path, r.status)
		}
	}
	if r := m.call(t, "GET", T+"/channels/prod/extensions", ci, ""); r.status != 200 || r.header.Get("Cache-Control") != "public, no-cache" {
		t.Errorf("the index: %d %v", r.status, r.header)
	}
	if r := m.call(t, "DELETE", T+"/channels/prod/extensions/newext/releases/x", ci, "", "If-Match", "*"); r.status != 401 {
		t.Errorf("purge: %d", r.status)
	}
	if r := m.upload(t, T+"/channels/prod/extensions/newext/releases?version=1.1&platform=linux_amd64", ci,
		built(t, "1.1", "newext_duckdb_cpp_init")); r.status != 404 {
		t.Errorf("publishing where it holds no publish: %d", r.status)
	}
	// another tenant does not know the key
	if r := m.call(t, "GET", "/api/v1/tenants/other/whoami", ci, ""); r.status != 401 {
		t.Errorf("another tenant: %d", r.status)
	}
	// rotation: a second key works beside the first; removing the first stops it at once
	ci2 := m.addKey(t, ta, "acme", "acl-ci")
	if r := m.call(t, "GET", S, ci2, ""); r.status != 200 {
		t.Errorf("the second key: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/publishers/acl-ci/keys/"+k.id, ta, ""); r.status != 204 {
		t.Fatalf("remove key: %d", r.status)
	}
	if r := m.call(t, "GET", S, ci, ""); r.status != 401 || !strings.Contains(string(r.body), k.fp) {
		t.Errorf("a removed key: %d %s", r.status, r.body)
	}
	// a removed publisher's keys stop at once, and its grants go
	if r := m.call(t, "DELETE", T+"/publishers/acl-ci", ta, ""); r.status != 204 {
		t.Fatalf("remove publisher: %d", r.status)
	}
	if r := m.call(t, "GET", S, ci2, ""); r.status != 401 {
		t.Errorf("after the publisher's removal: %d", r.status)
	}
	if got := m.call(t, "GET", T+"/grants?principal=publisher:acl-ci", ta, "").json(t)["grants"].([]any); len(got) != 0 {
		t.Errorf("a removed publisher's grants: %v", got)
	}
}

// Spec 0023: what a signed request must be. A malformed one, or one that names another request, is
// 400; a key or a signature that is not accepted, or a stale signed-at, is 401, naming the key and
// kista's clock; a body that is not the signed one is 400, with nothing written.
func TestSignedRefusals(t *testing.T) {
	m := newMgmt(t)
	ta := m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"
	m.call(t, "POST", T+"/publishers", ta, `{"name":"ext-ci"}`)
	m.call(t, "POST", T+"/grants", ta, `{"principal":"publisher:ext-ci","verbs":["publish"],"channel":"staging"}`)
	ci := m.addKey(t, ta, "acme", "ext-ci")
	k := m.signers[ci]
	S := T + "/channels/staging/extensions/newext/releases"
	pub := S + "?version=1.0&platform=linux_amd64"
	file := built(t, "1.0", "newext_duckdb_cpp_init")

	// headers signed for one request, sent with another
	signedFor := func(method, path string, body []byte) []string {
		st := auth.NewStatement(method, path, body, time.Now())
		sig, err := auth.SignStatement(k.s, st)
		if err != nil {
			t.Fatal(err)
		}
		return []string{auth.StatementHeader, base64.StdEncoding.EncodeToString(st), auth.SignatureHeader, base64.StdEncoding.EncodeToString(sig)}
	}
	send := func(method, path string, body io.Reader, length int64, hdr ...string) resp {
		req := httptest.NewRequest(method, path, body)
		req.ContentLength = length
		req.Header.Set("Content-Type", "application/octet-stream")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Add(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		m.h.ServeHTTP(rec, serve.WithHTTPS(req))
		return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
	}
	none := func() io.Reader { return bytes.NewReader(nil) }
	pubHdr := signedFor("POST", pub, file)
	getHdr := signedFor("GET", S, nil)
	other := append(append([]byte{}, file...), 0) // one byte more: another hash
	for name, tc := range map[string]struct {
		method, path string
		body         []byte
		hdr          []string
		want         int
		detail       string
	}{
		// each would be accepted were the check missing: the request itself is a valid one
		"another path":       {"GET", S, nil, signedFor("GET", T+"/whoami", nil), 400, "path"},
		"another method":     {"GET", S, nil, signedFor("POST", S, nil), 400, "method"},
		"HEAD for GET":       {"HEAD", S, nil, getHdr, 400, "method"},
		"another order":      {"POST", S + "?platform=linux_amd64&version=1.0", file, pubHdr, 400, "path"},
		"another body":       {"POST", pub, other, pubHdr, 400, "not the signed one"},
		"no body":            {"POST", pub, nil, pubHdr, 400, "not the signed one"},
		"no signature":       {"GET", S, nil, getHdr[:2], 400, "once each"},
		"twice":              {"GET", S, nil, append(append([]string{}, getHdr...), getHdr[:2]...), 400, "once each"},
		"with Authorization": {"GET", S, nil, append(append([]string{}, getHdr...), "Authorization", "Bearer "+ta), 400, "no Authorization"},
		"not base64":         {"GET", S, nil, []string{auth.StatementHeader, "!!", auth.SignatureHeader, getHdr[3]}, 400, "base64"},
		"bad signature":      {"GET", S, nil, []string{getHdr[0], getHdr[1], getHdr[2], pubHdr[3]}, 401, "kista's clock"},
	} {
		r := send(tc.method, tc.path, bytes.NewReader(tc.body), int64(len(tc.body)), tc.hdr...)
		if r.status != tc.want || tc.method != "HEAD" && !strings.Contains(string(r.body), tc.detail) {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
		if tc.want == 401 && !strings.Contains(string(r.body), k.fp) {
			t.Errorf("%s: the 401 does not name the key: %s", name, r.body)
		}
	}
	// HEAD signed as HEAD is a GET's answer
	if r := send("HEAD", S, none(), 0, signedFor("HEAD", S, nil)...); r.status != 200 {
		t.Errorf("HEAD: %d %s", r.status, r.body)
	}
	// a GET whose statement names a body
	if r := send("GET", S, none(), 0, signedFor("GET", S, []byte("x"))...); r.status != 400 {
		t.Errorf("a GET with a body's hash: %d", r.status)
	}
	// nothing was written by the bodies that were not the signed ones
	if got := m.call(t, "GET", S, ta, "").json(t)["releases"].([]any); len(got) != 0 {
		t.Fatalf("a release from a body that is not the signed one: %v", got)
	}
	// Expect: 100-continue: a refused request is answered before its body is read
	read := &watchReader{}
	if r := send("POST", pub, read, int64(len(file)), "Expect", "100-continue", pubHdr[0], pubHdr[1], pubHdr[2], getHdr[3]); r.status != 401 || read.n > 0 {
		t.Errorf("100-continue: %d, %d bytes read", r.status, read.n)
	}
	// signed-at more than five minutes away, either way
	for _, d := range []time.Duration{-6 * time.Minute, 6 * time.Minute} {
		k.at = time.Now().Add(d)
		if r := m.call(t, "GET", S, ci, ""); r.status != 401 {
			t.Errorf("signed-at %v away: %d", d, r.status)
		}
	}
	k.at = time.Now().Add(-4 * time.Minute)
	if r := m.call(t, "GET", S, ci, ""); r.status != 200 {
		t.Errorf("signed-at four minutes ago: %d %s", r.status, r.body)
	}
	k.at = time.Time{}
	// an unknown key, and an expired one (written directly: the service refuses a past expiry)
	unknown := m.hold(newSigner(t))
	if r := m.call(t, "GET", S, unknown, ""); r.status != 401 {
		t.Errorf("an unknown key: %d", r.status)
	}
	exp := newSigner(t)
	var pubID string
	ps, _ := m.adm.ListPublishers(context.Background(), admin, "acme")
	for _, p := range ps {
		if p.Name == "ext-ci" {
			pubID = p.ID
		}
	}
	tn, err := m.st.GetTenant(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.st.InTx(context.Background(), "", func(tx *store.Tx) error {
		return tx.InsertPublisherKey(context.Background(), &store.PublisherKey{TenantID: tn.ID, PublisherID: pubID, Type: "ssh-ed25519",
			PublicKey: strings.TrimSuffix(exp.authKey, " ci"), Fingerprint: exp.fp, ExpiresAt: time.Now().Add(-time.Minute), CreatedBy: "test"})
	}); err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "GET", S, m.hold(exp), ""); r.status != 401 {
		t.Errorf("an expired key: %d", r.status)
	}
	// a retired API key is an invalid token on the API, and no token on the DuckDB routes
	if r := m.call(t, "GET", S, "kista_0a1b2c3d_aaaa", ""); r.status != 401 {
		t.Errorf("a retired API key on the API: %d", r.status)
	}
	const aclPath = "/acme/prod/acl/1.0/v2.0.0/linux_amd64/acl.duckdb_extension.gz"
	if r, a := m.call(t, "GET", aclPath, "kista_0a1b2c3d_aaaa", ""), m.call(t, "GET", aclPath, "", ""); r.status != a.status {
		t.Errorf("a retired API key on a DuckDB route: %d, anonymous: %d", r.status, a.status)
	}
	// the removed routes
	for _, path := range []string{T + "/publishers/ext-ci/github", T + "/publishers/ext-ci/github/x"} {
		if r := m.call(t, "GET", path, ta, ""); r.status != 404 {
			t.Errorf("%s: %d", path, r.status)
		}
	}
}

// watchReader counts what is read of a body.
type watchReader struct{ n int }

func (w *watchReader) Read(p []byte) (int, error) {
	w.n += len(p)
	return 0, io.EOF
}

// Spec 0023: the same headers sent again (a replay within the five minutes) change nothing; a
// promotion's and an attachment's bodies are checked as a publication's.
func TestSignedReplayPromotionAttachment(t *testing.T) {
	m := newMgmt(t)
	ta := m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"
	m.call(t, "POST", T+"/publishers", ta, `{"name":"ext-ci"}`)
	m.call(t, "POST", T+"/grants", ta, `{"principal":"publisher:ext-ci","verbs":["publish"],"channel":"staging"}`)
	m.call(t, "POST", T+"/grants", ta, `{"principal":"publisher:ext-ci","verbs":["promote"],"channel":"prod","extension":"newext"}`)
	// attachments are an extension's, in every channel: a grant without a channel (spec 0011)
	m.call(t, "POST", T+"/grants", ta, `{"principal":"publisher:ext-ci","verbs":["publish"],"extension":"newext"}`)
	k := m.signers[m.addKey(t, ta, "acme", "ext-ci")]
	S := T + "/channels/staging/extensions/newext/releases"
	signedReq := func(method, path, ctype string, body []byte, hdr []string) resp {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Kista-Title", "SBOM") // an attachment's; other routes ignore it
		if hdr == nil {
			st := auth.NewStatement(method, path, body, time.Now())
			sig, err := auth.SignStatement(k.s, st)
			if err != nil {
				t.Fatal(err)
			}
			hdr = []string{auth.StatementHeader, base64.StdEncoding.EncodeToString(st), auth.SignatureHeader, base64.StdEncoding.EncodeToString(sig)}
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		m.h.ServeHTTP(rec, serve.WithHTTPS(req))
		return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
	}
	headersOf := func(method, path string, body []byte) []string {
		st := auth.NewStatement(method, path, body, time.Now())
		sig, _ := auth.SignStatement(k.s, st)
		return []string{auth.StatementHeader, base64.StdEncoding.EncodeToString(st), auth.SignatureHeader, base64.StdEncoding.EncodeToString(sig)}
	}
	seqs := func() string {
		var out []string
		for _, x := range m.call(t, "GET", S, ta, "").json(t)["releases"].([]any) {
			r := x.(map[string]any)
			out = append(out, fmt.Sprint(r["version"], ":", r["seq"]))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	// the same headers twice: 201, then the existing release, changing nothing (current included)
	f10 := built(t, "1.0", "newext_duckdb_cpp_init")
	h10 := headersOf("POST", S+"?version=1.0&platform=linux_amd64", f10)
	if r := signedReq("POST", S+"?version=1.0&platform=linux_amd64", "application/octet-stream", f10, h10); r.status != 201 {
		t.Fatalf("publish 1.0: %d %s", r.status, r.body)
	}
	if r := signedReq("POST", S+"?version=1.1&platform=linux_amd64", "application/octet-stream", built(t, "1.1", "newext_duckdb_cpp_init"), nil); r.status != 201 {
		t.Fatalf("publish 1.1: %d %s", r.status, r.body)
	}
	before := seqs()
	if r := signedReq("POST", S+"?version=1.0&platform=linux_amd64", "application/octet-stream", f10, h10); r.status != 200 {
		t.Fatalf("the same headers again: %d %s", r.status, r.body)
	}
	if after := seqs(); after != before {
		t.Fatalf("a replay changed the channel: %s, then %s", before, after)
	}
	// a promotion: its JSON is the signed body
	P := T + "/channels/prod/extensions/newext/releases/promote"
	signed10 := []byte(`{"from_channel":"staging","version":"1.0"}`)
	h := headersOf("POST", P, signed10)
	if r := signedReq("POST", P, "application/json", []byte(`{"from_channel":"staging","version":"1.1"}`), h); r.status != 400 ||
		!strings.Contains(string(r.body), "not the signed one") {
		t.Fatalf("a promotion of another version: %d %s", r.status, r.body)
	}
	if got := m.call(t, "GET", T+"/channels/prod/extensions/newext/releases", ta, "").json(t)["releases"].([]any); len(got) != 0 {
		t.Fatalf("promoted with a body that is not the signed one: %v", got)
	}
	if r := signedReq("POST", P, "application/json", signed10, h); r.status != 201 {
		t.Fatalf("promote: %d %s", r.status, r.body)
	}
	if r := signedReq("POST", P, "application/json", signed10, h); r.status != 200 {
		t.Fatalf("the promotion's headers again: %d %s", r.status, r.body)
	}
	// the signed request is the administrators' to see, not another publisher's
	rel := m.call(t, "GET", T+"/channels/prod/extensions/newext/releases", ta, "").json(t)["releases"].([]any)[0].(map[string]any)
	if rel["publisher_statement"] == nil {
		t.Fatalf("the administrator does not see the signed request: %v", rel)
	}
	if got := m.call(t, "GET", S, m.hold(k), "").json(t)["releases"].([]any)[0].(map[string]any); got["publisher_statement"] != nil {
		t.Fatalf("a publisher sees the signed request: %v", got)
	}
	// an attachment: the document is the signed body
	var body string
	for _, x := range m.call(t, "GET", S, ta, "").json(t)["releases"].([]any) {
		if r := x.(map[string]any); r["version"] == "1.0" {
			body = r["body_hash"].(string)
		}
	}
	A := T + "/extensions/newext/attachments?body_hash=" + body + "&kind=sbom"
	sbom := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.5","components":[]}`)
	ha := headersOf("POST", A, sbom)
	if r := signedReq("POST", A, "application/vnd.cyclonedx+json", []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`), ha); r.status != 400 ||
		!strings.Contains(string(r.body), "not the signed one") {
		t.Fatalf("an attachment that is not the signed one: %d %s", r.status, r.body)
	}
	if r := signedReq("POST", A, "application/vnd.cyclonedx+json", sbom, ha); r.status != 201 {
		t.Fatalf("attach by a signed request: %d %s", r.status, r.body)
	}
	if got := m.call(t, "GET", T+"/extensions/newext/attachments", ta, "").json(t)["attachments"].([]any); len(got) != 1 {
		t.Fatalf("attachments: %v", got)
	}
}
