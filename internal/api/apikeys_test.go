package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0008 phase 2: a publisher's API key publishes and promotes, and is a token nowhere else.
func TestAPIKeyPublishing(t *testing.T) {
	m := newMgmt(t)
	ta := m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"
	m.call(t, "POST", T+"/publishers", ta, `{"name":"ext-ci"}`)
	if r := m.call(t, "POST", T+"/grants", ta, `{"principal":"publisher:ext-ci","verbs":["publish"],"channel":"staging"}`); r.status != 201 {
		t.Fatalf("grant: %d %s", r.status, r.body)
	}
	m.call(t, "POST", T+"/grants", ta, `{"principal":"publisher:ext-ci","verbs":["promote"],"channel":"prod","extension":"newext"}`)
	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for name, body := range map[string]string{
		"no expiry":   `{}`,
		"past":        `{"expires_at":"2001-01-01T00:00:00Z"}`,
		"over a year": `{"expires_at":"` + time.Now().Add(400*24*time.Hour).UTC().Format(time.RFC3339) + `"}`,
		"not a time":  `{"expires_at":"soon"}`,
	} {
		if r := m.call(t, "POST", T+"/publishers/ext-ci/keys", ta, body); r.status != 400 {
			t.Errorf("%s: %d", name, r.status)
		}
	}
	r := m.call(t, "POST", T+"/publishers/ext-ci/keys", ta, `{"expires_at":"`+exp+`"}`)
	if r.status != 201 || r.json(t)["key"] == nil {
		t.Fatalf("issue a key: %d %s", r.status, r.body)
	}
	key := r.json(t)["key"].(string)
	keyID := r.json(t)["id"].(string)
	// the key appears once: lists show its prefix only
	if l := m.call(t, "GET", T+"/publishers/ext-ci/keys", ta, ""); strings.Contains(string(l.body), key) || len(l.json(t)["keys"].([]any)) != 1 {
		t.Fatalf("key list: %s", l.body)
	}
	S := T + "/channels/staging/extensions/newext/releases"
	r = m.upload(t, S+"?version=1.0&platform=linux_amd64", key, built(t, "1.0", "newext_duckdb_cpp_init"))
	if r.status != 201 {
		t.Fatalf("publish with a key: %d %s", r.status, r.body)
	}
	if strings.Contains(string(r.body), key) || strings.Contains(string(r.body), auth.APIKeyHash(key)) {
		t.Fatalf("the key in the publication's answer: %s", r.body)
	}
	if prov := r.json(t)["provenance"].(map[string]any); fmt.Sprint(prov["credentials"]) != "[key:"+keyID+"]" {
		t.Errorf("provenance: %v", prov)
	}
	w := m.call(t, "GET", T+"/whoami", key, "").json(t)
	if pubs, _ := w["publishers"].([]any); w["provider"] != "apikey" || len(pubs) != 1 || len(w["grants"].([]any)) != 2 ||
		pubs[0].(map[string]any)["credential"] != "key:"+keyID {
		t.Errorf("whoami: %v", w)
	}
	// and promotes
	if r := m.call(t, "POST", T+"/channels/prod/extensions/newext/releases/promote", key, `{"from_channel":"staging","version":"1.0"}`); r.status != 201 {
		t.Fatalf("promote with a key: %d %s", r.status, r.body)
	}
	// nor purges (spec 0016): an administrator's change
	if r := m.call(t, "DELETE", T+"/channels/prod/extensions/newext/releases/x", key, "", "If-Match", "*"); r.status != 401 {
		t.Errorf("purge with a key: %d", r.status)
	}
	// a key is a token nowhere else: the index, management, server routes, another tenant
	for _, path := range []string{T + "/channels/staging/extensions", T + "/grants", "/api/v1/info", "/api/v1/tenants",
		"/api/v1/tenants/other/channels/prod/extensions/newext/releases"} {
		if r := m.call(t, "GET", path, key, ""); r.status != 401 {
			t.Errorf("%s with a key: %d", path, r.status)
		}
	}
	// the DuckDB routes take it as no token (a private release is 401 to anonymous)
	const aclPath = "/acme/prod/acl/1.0/v2.0.0/linux_amd64/acl.duckdb_extension.gz"
	if r, a := m.call(t, "GET", aclPath, key, ""), m.call(t, "GET", aclPath, "", ""); r.status != a.status {
		t.Errorf("a key on a DuckDB route: %d, anonymous: %d", r.status, a.status)
	}
	// a malformed or unknown key
	unknown, _, _, _ := auth.NewAPIKey()
	for _, bad := range []string{"kista_nope", unknown} {
		if r := m.call(t, "GET", S, bad, ""); r.status != 401 {
			t.Errorf("%q: %d", bad, r.status)
		}
	}
	// rotation: a second key works while the first does; removing the first stops it at once
	r = m.call(t, "POST", T+"/publishers/ext-ci/keys", ta, `{"expires_at":"`+exp+`"}`)
	key2 := r.json(t)["key"].(string)
	if r := m.call(t, "GET", S, key2, ""); r.status != 200 {
		t.Errorf("the second key: %d", r.status)
	}
	if r := m.call(t, "DELETE", T+"/publishers/ext-ci/keys/"+keyID, ta, ""); r.status != 204 {
		t.Fatalf("remove key: %d", r.status)
	}
	if r := m.call(t, "GET", S, key, ""); r.status != 401 {
		t.Errorf("a removed key: %d", r.status)
	}
	// an expired key (written directly: the service refuses a past expiry)
	k, prefix, hash, _ := auth.NewAPIKey()
	var pubID string
	ps, _ := m.adm.ListPublishers(context.Background(), admin, "acme")
	for _, p := range ps {
		if p.Name == "ext-ci" {
			pubID = p.ID
		}
	}
	if err := m.st.InTx(context.Background(), "", func(tx *store.Tx) error {
		return tx.InsertAPIKey(context.Background(), &store.APIKey{PublisherID: pubID, Prefix: prefix, Hash: hash,
			ExpiresAt: time.Now().Add(-time.Minute), CreatedBy: "test"})
	}); err != nil {
		t.Fatal(err)
	}
	if r := m.call(t, "GET", S, k, ""); r.status != 401 {
		t.Errorf("an expired key: %d", r.status)
	}
	// a removed publisher's keys stop at once
	if r := m.call(t, "DELETE", T+"/publishers/ext-ci", ta, ""); r.status != 204 {
		t.Fatalf("remove publisher: %d", r.status)
	}
	if r := m.call(t, "GET", S, key2, ""); r.status != 401 {
		t.Errorf("a removed publisher's key: %d", r.status)
	}
}
