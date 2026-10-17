package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// The bundle upload routes (spec 0014 phase 2): a server administrator's; a chunk with its RFC 9530
// Content-Digest; complete; abort; the import records.
func TestUploadRoutes(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	tom, root := m.toks["tenant admin"], m.toks["server admin"]
	if r := m.call(t, "POST", T+"/bundle-uploads", tom, `{"size":10}`); r.status != 404 {
		t.Fatalf("a tenant administrator creates an upload: %d", r.status)
	}
	r := m.call(t, "POST", T+"/bundle-uploads", root, `{"size":10}`)
	if r.status != 201 || r.json(t)["chunks"] != float64(1) || r.json(t)["state"] != "uploading" {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	id := r.json(t)["id"].(string)
	chunk := "0123456789"
	sum := sha256.Sum256([]byte(chunk))
	digest := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
	put := T + "/bundle-uploads/" + id + "/chunks/0"
	if r := m.call(t, "PUT", put, root, chunk); r.status != 400 {
		t.Fatalf("no digest: %d %s", r.status, r.body)
	}
	if r := m.call(t, "PUT", put, root, chunk, "Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(make([]byte, 32))+":"); r.status != 400 {
		t.Fatalf("a wrong digest: %d %s", r.status, r.body)
	}
	if r := m.call(t, "PUT", T+"/bundle-uploads/"+id+"/chunks/01", root, chunk, "Content-Digest", digest); r.status != 404 {
		t.Fatalf("a chunk number not canonical: %d", r.status)
	}
	if r := m.call(t, "PUT", put, tom, chunk, "Content-Digest", digest); r.status != 404 {
		t.Fatalf("a tenant administrator's chunk: %d", r.status)
	}
	if r := m.call(t, "PUT", put, root, chunk, "Content-Digest", digest); r.status != 204 {
		t.Fatalf("a chunk: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/bundle-uploads/"+id, root, ""); r.status != 200 || len(r.json(t)["received"].([]any)) != 1 {
		t.Fatalf("get: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/bundle-uploads/"+id+"/complete", root, ""); r.status != 202 || r.json(t)["state"] != "queued" {
		t.Fatalf("complete: %d %s", r.status, r.body)
	}
	if r := m.call(t, "PUT", put, root, chunk, "Content-Digest", digest); r.status != 409 || strings.Contains(string(r.body), "store:") {
		t.Fatalf("a chunk after complete: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/bundle-uploads/"+id, root, ""); r.status != 204 {
		t.Fatalf("abort: %d %s", r.status, r.body)
	}
	if r := m.call(t, "DELETE", T+"/bundle-uploads/"+id, root, ""); r.status != 409 {
		t.Fatalf("abort again: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/bundle-uploads", root, ""); r.status != 200 || len(r.json(t)["uploads"].([]any)) != 1 {
		t.Fatalf("list: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/bundle-imports", root, ""); r.status != 200 {
		t.Fatalf("imports: %d %s", r.status, r.body)
	}
	if r := m.call(t, "GET", T+"/bundle-imports/nope", root, ""); r.status != 404 {
		t.Fatalf("an unknown import: %d", r.status)
	}
}
