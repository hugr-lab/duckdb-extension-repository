package api_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/symbols/symtest"
)

// built is an extension file whose binary exports the given entry points (an x86-64 ELF), so it
// passes spec 0008's checks for linux_amd64.
func built(t *testing.T, version string, exports ...string) []byte {
	t.Helper()
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0",
		ExtensionVersion: version, ABI: extfile.ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	b := append(symtest.ELF(exports...), extfile.MetadataPrefix...)
	b = append(b, block[:]...)
	return append(b, make([]byte, extfile.SignatureSize)...)
}

// upload POSTs a file to the publication route.
func (m *mgmt) upload(t *testing.T, path, token string, file []byte, hdr ...string) resp {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewReader(file))
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Content-Length" {
			req.ContentLength, _ = strconv.ParseInt(hdr[i+1], 10, 64)
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	m.h.ServeHTTP(rec, serve.WithHTTPS(req))
	return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
}

func TestPublication(t *testing.T) {
	m := newMgmt(t)
	const P = "/api/v1/tenants/acme/channels/prod/extensions/"
	grant := func(sub, verb, channel, ext string) {
		if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|"+sub, []string{verb}, channel, ext); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(sub string) string {
		return m.keys.sign(t, idpURL, map[string]any{"sub": sub, "aud": publicURL + "/acme"})
	}
	grant("pat", "publish", "prod", "newext") // one extension in one channel
	grant("cora", "publish", "prod", "")      // channel-wide
	grant("hana", "publish", "", "httpfs")    // a reserved name, named
	file := built(t, "1.0", "newext_duckdb_cpp_init")
	q := "/releases?version=1.0&platform=linux_amd64"

	// who: publish on the name; an administrator without the verb is not enough
	for caller, want := range map[string]int{"": 401, "tom": 404, "ivan": 404, "carl": 404, "eve": 404} {
		token := ""
		if caller != "" {
			token = m.toks[map[string]string{"tom": "tenant admin", "ivan": "install", "carl": "channel adm", "eve": "ext admin"}[caller]]
		}
		if r := m.upload(t, P+"newext"+q, token, file); r.status != want {
			t.Errorf("%s: %d, want %d", caller, r.status, want)
		}
	}
	// the request's checks before anything is read
	for name, tc := range map[string]struct {
		path string
		hdr  []string
		want int
	}{
		"unknown parameter": {P + "newext" + q + "&x=1", nil, 400},
		"no version":        {P + "newext/releases?platform=linux_amd64", nil, 400},
		"bad visibility":    {P + "newext" + q + "&visibility=world", nil, 400},
		"media type":        {P + "newext" + q, []string{"Content-Type", "application/json"}, 415},
		"no length":         {P + "newext" + q, []string{"Content-Length", "-1"}, 411},
		"too large":         {P + "newext" + q, []string{"Content-Length", strconv.Itoa(2 << 30)}, 413},
		"declared version":  {P + "newext/releases?version=9.9&platform=linux_amd64", nil, 400},
		"declared platform": {P + "newext/releases?version=1.0&platform=linux_arm64", nil, 400},
		"another extension": {P + "otherext" + q, nil, 404}, // pat's grant names newext
		"another channel":   {"/api/v1/tenants/acme/channels/staging/extensions/newext" + q, nil, 404},
	} {
		if r := m.upload(t, tc.path, tok("pat"), file, tc.hdr...); r.status != tc.want {
			t.Errorf("%s: %d, want %d: %s", name, r.status, tc.want, r.body)
		}
	}
	// a binary built as another extension
	if r := m.upload(t, P+"newext"+q, tok("pat"), built(t, "1.0", "other_duckdb_cpp_init")); r.status != 400 ||
		!strings.Contains(string(r.body), "entry point") {
		t.Errorf("another binary: %d %s", r.status, r.body)
	}

	// published: 201, private by default, Location readable by the publisher (without who changed it)
	r := m.upload(t, P+"newext"+q, tok("pat"), file)
	if r.status != 201 || r.header.Get("Location") == "" {
		t.Fatalf("publish: %d %s", r.status, r.body)
	}
	rel := r.json(t)
	if rel["visibility"] != "private" || rel["origin"] != "publication" || rel["created_by"] != nil || rel["provenance"] == nil {
		t.Fatalf("the release: %v", rel)
	}
	if g := m.call(t, "GET", r.header.Get("Location"), tok("pat"), ""); g.status != 200 {
		t.Fatalf("read back: %d", g.status)
	}
	if again := m.upload(t, P+"newext"+q, tok("pat"), file); again.status != 200 || again.json(t)["id"] != rel["id"] {
		t.Fatalf("publish again: %d %s", again.status, again.body)
	}
	// other choices for the same slot
	if r := m.upload(t, P+"newext"+q+"&visibility=public", tok("pat"), file); r.status != 409 {
		t.Errorf("other choices: %d %s", r.status, r.body)
	}
	// a stale token does not publish
	stale := m.keys.sign(t, idpURL, map[string]any{"sub": "pat", "aud": publicURL + "/acme", "iat": time.Now().Add(-2 * time.Hour).Unix()})
	if r := m.upload(t, P+"newext"+q, stale, built(t, "1.1", "newext_duckdb_cpp_init")); r.status != 401 {
		t.Errorf("stale: %d", r.status)
	}

	// reserved: a channel-wide grant does not reach httpfs; a grant naming it does
	hf := built(t, "1.0", "httpfs_duckdb_cpp_init")
	if r := m.upload(t, P+"httpfs"+q, tok("cora"), hf); r.status != 404 {
		t.Errorf("httpfs with a channel-wide grant: %d", r.status)
	}
	if r := m.upload(t, P+"anyname"+q, tok("cora"), built(t, "1.0", "anyname_duckdb_cpp_init")); r.status != 201 {
		t.Errorf("a channel-wide grant on another name: %d %s", r.status, r.body)
	}
	r = m.upload(t, P+"httpfs"+q+"&visibility=public", tok("hana"), hf)
	if r.status != 201 || r.json(t)["shadows"] != "core" {
		t.Fatalf("httpfs with a grant naming it: %d %s", r.status, r.body)
	}
	if row := m.call(t, "GET", "/api/v1/tenants/acme/channels/prod/extensions/httpfs", "", "").json(t)["releases"].([]any)[0].(map[string]any); row["shadows"] != "core" || row["origin"] != "publication" {
		t.Errorf("httpfs in the index: %v", row)
	}
	// promoting a reserved name needs grants naming it on both sides
	grant("pro", "promote", "staging", "")       // channel-wide promote
	grant("pro", "publish", "prod", "httpfs")    // named publish on the source
	grant("prn", "promote", "staging", "httpfs") // named promote
	grant("prn", "publish", "prod", "httpfs")
	body := `{"from_channel":"prod","version":"1.0"}`
	const SP = "/api/v1/tenants/acme/channels/staging/extensions/httpfs/releases/promote"
	if r := m.call(t, "POST", SP, tok("pro"), body); r.status != 404 {
		t.Errorf("a channel-wide promote of httpfs: %d", r.status)
	}
	if r := m.call(t, "POST", SP, tok("prn"), body); r.status != 201 {
		t.Errorf("a named promote of httpfs: %d %s", r.status, r.body)
	}
}

func TestPromotionAndBlocks(t *testing.T) {
	m := newMgmt(t)
	grant := func(sub, verb, channel, ext string) {
		if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|"+sub, []string{verb}, channel, ext); err != nil {
			t.Fatal(err)
		}
	}
	tok := func(sub string) string {
		return m.keys.sign(t, idpURL, map[string]any{"sub": sub, "aud": publicURL + "/acme"})
	}
	grant("ci", "publish", "staging", "newext")
	grant("rm", "promote", "prod", "newext")
	grant("rm", "publish", "staging", "newext")
	grant("rp", "promote", "prod", "newext") // promote without publish on the source
	file := built(t, "1.0", "newext_duckdb_cpp_init")
	S := "/api/v1/tenants/acme/channels/staging/extensions/newext/releases"
	r := m.upload(t, S+"?version=1.0&platform=linux_amd64&visibility=public", tok("ci"), file)
	if r.status != 201 {
		t.Fatalf("publish to staging: %d %s", r.status, r.body)
	}
	hash := r.json(t)["body_hash"].(string)
	// the release manager finds it, then promotes the version
	if got := m.call(t, "GET", S, tok("rm"), "").json(t)["releases"].([]any); len(got) != 1 {
		t.Fatalf("staging releases for rm: %v", got)
	}
	const PR = "/api/v1/tenants/acme/channels/prod/extensions/newext/releases/promote"
	if r := m.call(t, "POST", PR, tok("rp"), `{"from_channel":"staging","version":"1.0"}`); r.status != 404 {
		t.Errorf("promote without publish on the source: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", PR, tok("ci"), `{"from_channel":"staging","version":"1.0"}`); r.status != 404 {
		t.Errorf("promote without promote: %d", r.status)
	}
	if r := m.call(t, "POST", PR, tok("rm"), `{"from_channel":"staging","version":"1.0","visibility":"public"}`); r.status != 400 {
		t.Errorf("widening visibility: %d", r.status)
	}
	r = m.call(t, "POST", PR, tok("rm"), `{"from_channel":"staging","version":"1.0"}`)
	if r.status != 201 {
		t.Fatalf("promote: %d %s", r.status, r.body)
	}
	rels := r.json(t)["releases"].([]any)
	if len(rels) != 1 || rels[0].(map[string]any)["origin"] != "promotion" || rels[0].(map[string]any)["visibility"] != "public" {
		t.Fatalf("promoted: %v", rels)
	}
	if r := m.call(t, "POST", PR, tok("rm"), `{"from_channel":"staging","version":"1.0"}`); r.status != 200 {
		t.Errorf("promote again: %d", r.status)
	}
	// the index: origin for everyone, provenance for the extension's publishers
	idx := "/api/v1/tenants/acme/channels/prod/extensions/newext"
	row := m.call(t, "GET", idx, "", "").json(t)["releases"].([]any)[0].(map[string]any)
	if row["origin"] != "promotion" || row["provenance"] != nil {
		t.Errorf("anonymous row: %v", row)
	}
	if row := m.call(t, "GET", idx, tok("rm"), "").json(t)["releases"].([]any)[0].(map[string]any); row["provenance"] == nil {
		t.Errorf("rm's row: %v", row)
	}

	// block: the tenant administrator bans the body; both channels' releases are yanked
	ta := m.toks["tenant admin"]
	const B = "/api/v1/tenants/acme/blocks"
	if r := m.call(t, "POST", B, tok("rm"), `{"body_hash":"`+hash+`","reason":"x"}`); r.status != 404 {
		t.Errorf("block by a publisher: %d", r.status)
	}
	r = m.call(t, "POST", B, ta, `{"body_hash":"`+hash+`","reason":"CVE-1"}`)
	if r.status != 201 || r.header.Get("Location") != B+"/"+hash {
		t.Fatalf("block: %d %v %s", r.status, r.header, r.body)
	}
	row = m.call(t, "GET", idx, "", "").json(t)["releases"].([]any)[0].(map[string]any)
	if row["state"] != "yanked" || row["blocked"] != true {
		t.Errorf("blocked row: %v", row)
	}
	if r := m.upload(t, S+"?version=1.0&platform=linux_amd64", tok("ci"), file); r.status != 409 {
		t.Errorf("publishing a blocked body: %d %s", r.status, r.body)
	}
	if got := m.call(t, "GET", B, ta, "").json(t)["blocks"].([]any); len(got) != 1 {
		t.Errorf("blocks: %v", got)
	}
	if r := m.call(t, "GET", B+"/"+hash, ta, ""); r.status != 200 || r.json(t)["reason"] != "CVE-1" {
		t.Errorf("get block: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", B+"/"+hash, ta, ""); r.status != 204 {
		t.Errorf("unblock: %d", r.status)
	}
	if r := m.call(t, "DELETE", B+"/"+hash, ta, ""); r.status != 404 {
		t.Errorf("unblock again: %d", r.status)
	}
}

// Uploads run at most publish.max_per_principal at once per principal.
func TestUploadConcurrency(t *testing.T) {
	m := newMgmt(t)
	if _, err := m.env.adm.AddGrant(ctx, admin, "acme", "subject:corp|pat", []string{"publish"}, "prod", ""); err != nil {
		t.Fatal(err)
	}
	tok := m.keys.sign(t, idpURL, map[string]any{"sub": "pat", "aud": publicURL + "/acme"})
	const P = "/api/v1/tenants/acme/channels/prod/extensions/"
	pr, pw := io.Pipe()
	done := make(chan int)
	started := make(chan struct{}, 1) // buffered: the reader may signal before the test waits
	go func() {
		req := httptest.NewRequest("POST", P+"a1/releases?version=1.0&platform=linux_amd64", io.TeeReader(pr, notify{started}))
		req.ContentLength = 4096
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		m.h.ServeHTTP(rec, serve.WithHTTPS(req))
		done <- rec.Code
	}()
	_, _ = pw.Write([]byte{1})
	<-started
	r1 := make(chan int)
	pr2, pw2 := io.Pipe()
	started2 := make(chan struct{}, 1)
	go func() {
		req := httptest.NewRequest("POST", P+"a2/releases?version=1.0&platform=linux_amd64", io.TeeReader(pr2, notify{started2}))
		req.ContentLength = 4096
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		m.h.ServeHTTP(rec, serve.WithHTTPS(req))
		r1 <- rec.Code
	}()
	_, _ = pw2.Write([]byte{1})
	<-started2
	// two uploads hold pat's slots: a third answers 429
	if r := m.upload(t, P+"a3/releases?version=1.0&platform=linux_amd64", tok, built(t, "1.0", "a3_duckdb_cpp_init")); r.status != http.StatusTooManyRequests ||
		r.header.Get("Retry-After") == "" {
		t.Errorf("a third upload: %d", r.status)
	}
	_ = pw.Close()
	_ = pw2.Close()
	<-done
	<-r1
}

type notify struct{ c chan struct{} }

func (n notify) Write(p []byte) (int, error) {
	select {
	case n.c <- struct{}{}:
	default:
	}
	return len(p), nil
}
