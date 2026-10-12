package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Attachments over the API (spec 0011 phase 1a): attached by a tenant administrator, refused to a
// channel-scoped one, readers-visible only on purpose, read through the index in the caller's view,
// summarised on the index rows, downloaded as files.
func TestAttachmentRoutes(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	tom, eve, anon := m.toks["tenant admin"], m.toks["ext admin"], ""
	// tresor's public 1.0 in prod: its body hash from the index
	rows := m.call(t, "GET", T+"/channels/prod/extensions/tresor", anon, "").json(t)["releases"].([]any)
	body := ""
	for _, x := range rows {
		if r := x.(map[string]any); r["version"] == "1.0" {
			body = r["body_hash"].(string)
		}
	}
	if body == "" {
		t.Fatalf("no tresor 1.0: %v", rows)
	}
	sbom := `{"bomFormat":"CycloneDX","specVersion":"1.5","components":[{"type":"library"},{"type":"library"}]}`
	attach := func(tok, ext, q, mt, doc string) resp {
		return m.call(t, "POST", T+"/extensions/"+ext+"/attachments?"+q, tok, doc, "Content-Type", mt, "Kista-Title", "SBOM%20%D1%81%D0%B1%D0%BE%D1%80%D0%BA%D0%B8")
	}
	// a channel-scoped extension administrator cannot attach: it would show in every channel
	if r := attach(eve, "tresor", "body_hash="+body+"&kind=sbom", "application/vnd.cyclonedx+json", sbom); r.status != 404 {
		t.Fatalf("eve: %d %s", r.status, r.body)
	}
	r := attach(tom, "tresor", "body_hash="+body+"&kind=sbom", "application/vnd.cyclonedx+json", sbom)
	if r.status != 201 || !strings.HasPrefix(r.header.Get("Location"), T+"/extensions/tresor/attachments/") {
		t.Fatalf("attach: %d %s", r.status, r.body)
	}
	doc := r.json(t)
	id, etag := doc["id"].(string), r.header.Get("ETag")
	if doc["visibility"] != "administrators" || doc["title"] != "SBOM сборки" || doc["summary"].(map[string]any)["components"] != 2.0 {
		t.Fatalf("attached: %v", doc)
	}
	for q, want := range map[string]int{
		"body_hash=" + body + "&kind=sbom&extra=1":            400,
		"body_hash=" + strings.Repeat("c", 64) + "&kind=sbom": 400, // not one of tresor's bodies
		"body_hash=" + body + "&kind=virus":                   400,
	} {
		if r := attach(tom, "tresor", q, "application/vnd.cyclonedx+json", sbom); r.status != want {
			t.Errorf("%s: %d %s", q, r.status, r.body)
		}
	}
	// readers see nothing until it is made readers-visible
	idx := T + "/channels/prod/extensions/tresor/attachments"
	if got := m.call(t, "GET", idx, anon, "").json(t)["attachments"].([]any); len(got) != 0 {
		t.Fatalf("administrators-only shown to readers: %v", got)
	}
	if r := m.call(t, "POST", T+"/extensions/tresor/attachments/"+id+"/readers", tom, ""); r.status != 428 {
		t.Fatalf("without If-Match: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/extensions/tresor/attachments/"+id+"/readers", eve, "", "If-Match", etag); r.status != 404 {
		t.Fatalf("eve changes visibility: %d", r.status)
	}
	if r := m.call(t, "POST", T+"/extensions/tresor/attachments/"+id+"/readers", tom, "", "If-Match", etag); r.status != 200 {
		t.Fatalf("readers: %d %s", r.status, r.body)
	}
	got := m.call(t, "GET", idx, anon, "").json(t)["attachments"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["attached_by"] != nil || got[0].(map[string]any)["visibility"] != nil {
		t.Fatalf("readers' list: %v", got)
	}
	// the download: a file, never rendered
	dl := m.call(t, "GET", idx+"/"+id, anon, "")
	if dl.status != 200 || string(dl.body) != sbom || dl.header.Get("Content-Type") != "application/vnd.cyclonedx+json" ||
		dl.header.Get("Content-Disposition") != `attachment; filename="`+id+`.json"` || dl.header.Get("X-Content-Type-Options") != "nosniff" ||
		dl.header.Get("Content-Security-Policy") != "sandbox" || dl.header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("download: %d %v", dl.status, dl.header)
	}
	// the index rows carry the summary per kind, for the body's rows only
	rows = m.call(t, "GET", T+"/channels/prod/extensions/tresor", anon, "").json(t)["releases"].([]any)
	seen := 0
	for _, x := range rows {
		r := x.(map[string]any)
		a, ok := r["attachments"].(map[string]any)
		if (r["body_hash"] == body) != ok {
			t.Fatalf("row %v: %v", r["version"], a)
		}
		if ok {
			seen++
			if s := a["sbom"].(map[string]any); s["count"] != 1.0 || s["latest"].(map[string]any)["components"] != 2.0 {
				t.Fatalf("summary: %v", s)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no row carried the summary")
	}
	// the management list: administrators (eve is channel-scoped: no)
	if r := m.call(t, "GET", T+"/extensions/tresor/attachments", eve, ""); r.status != 404 {
		t.Fatalf("eve lists: %d", r.status)
	}
	if r := m.call(t, "GET", T+"/extensions/tresor/attachments/"+id+"/content", tom, ""); r.status != 200 || string(r.body) != sbom {
		t.Fatalf("content: %d", r.status)
	}
	var list struct {
		Attachments []map[string]any `json:"attachments"`
	}
	if err := json.Unmarshal(m.call(t, "GET", T+"/extensions/tresor/attachments?kind=sbom", tom, "").body, &list); err != nil ||
		len(list.Attachments) != 1 || list.Attachments[0]["head"] != true || list.Attachments[0]["attached_by"] == nil {
		t.Fatalf("management list: %v %v", list, err)
	}
	// removal: then readers see nothing, and the download is gone
	if r := m.call(t, "DELETE", T+"/extensions/tresor/attachments/"+id, tom, ""); r.status != http.StatusNoContent {
		t.Fatalf("remove: %d", r.status)
	}
	if r := m.call(t, "GET", idx+"/"+id, anon, ""); r.status != 404 {
		t.Fatalf("a removed download: %d", r.status)
	}
}
