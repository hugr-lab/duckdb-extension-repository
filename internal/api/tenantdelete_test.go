package api_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0019 over the API: only a suspended tenant is deleted, by a server administrator with an
// exact If-Match; from then on it is unknown to everyone but server administrators, who see its
// record, its deletion and its events.
func TestDeleteTenantRoute(t *testing.T) {
	m := newMgmt(t)
	sa, oa := m.toks["server admin"], m.toks["other admin"]
	const T = "/api/v1/tenants/other"
	tag := func() string {
		t.Helper()
		r := m.call(t, "GET", T, sa, "")
		if r.status != 200 {
			t.Fatalf("get: %d %s", r.status, r.body)
		}
		return r.header.Get("ETag")
	}
	if r := m.call(t, "DELETE", T, oa, "", "If-Match", tag()); r.status != 404 {
		t.Errorf("a tenant administrator deletes its tenant: %d", r.status)
	}
	if r := m.call(t, "DELETE", T, sa, "", "If-Match", "*"); r.status != 428 {
		t.Errorf("If-Match *: %d", r.status)
	}
	if r := m.call(t, "DELETE", T, sa, "", "If-Match", tag()); r.status != 409 || !strings.Contains(string(r.body), "suspend it first") {
		t.Errorf("an active tenant: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/suspend", sa, "", "If-Match", tag()); r.status != 200 {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
	r := m.call(t, "GET", T, sa, "")
	if j := r.json(t); j["suspended_at"] == nil || j["deletable_at"] == nil {
		t.Errorf("a suspended tenant's record: %s", r.body)
	}
	if r := m.call(t, "DELETE", T, sa, "", "If-Match", `"v99"`); r.status != 412 {
		t.Errorf("a stale If-Match: %d", r.status)
	}
	r = m.call(t, "DELETE", T, sa, "", "If-Match", tag())
	if r.status != 202 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	j := r.json(t)
	del, _ := j["deletion"].(map[string]any)
	if j["state"] != store.TenantDeleting || del == nil || del["started_by"] == "" || del["keys"] == nil {
		t.Fatalf("the deleting tenant: %s", r.body)
	}
	if r := m.call(t, "DELETE", T, sa, "", "If-Match", tag()); r.status != 409 || !strings.Contains(string(r.body), "being deleted") {
		t.Errorf("deleting again: %d %s", r.status, r.body)
	}

	// unknown to everyone else, exactly as a name that never existed; server administrators see the
	// record and the events only
	for _, who := range []string{"anonymous", "other admin", "server admin"} {
		for _, path := range []string{"", "/channels", "/issuers", "/grants", "/attachments"} {
			if who == "server admin" && path == "" {
				continue
			}
			gone, never := m.call(t, "GET", T+path, m.toks[who], ""), m.call(t, "GET", "/api/v1/tenants/never"+path, m.toks[who], "")
			if gone.status != never.status || gone.status < 400 || !bytes.Equal(gone.body, never.body) ||
				gone.header.Get("Cache-Control") != never.header.Get("Cache-Control") {
				t.Errorf("%s GET %s: %d %s, unknown: %d %s", who, path, gone.status, gone.body, never.status, never.body)
			}
		}
	}
	if r := m.call(t, "GET", T+"/events", sa, ""); r.status != 200 || !strings.Contains(string(r.body), "tenant.delete") {
		t.Errorf("events of a deleting tenant: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/tenants?state=deleting", sa, ""); r.status != 200 || !strings.Contains(string(r.body), `"other"`) ||
		strings.Contains(string(r.body), `"acme"`) {
		t.Errorf("?state=deleting: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", "/api/v1/tenants?state=gone", sa, ""); r.status != 400 {
		t.Errorf("?state=gone: %d", r.status)
	}
	if r := m.call(t, "POST", "/api/v1/tenants", sa, `{"name":"other"}`); r.status != 409 || !strings.Contains(string(r.body), "never reused") {
		t.Errorf("the name again: %d %s", r.status, r.body)
	}
}
