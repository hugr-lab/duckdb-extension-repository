package serve

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

type fakeEvents struct {
	mu      sync.Mutex
	events  []string // kind actor subject
	data    []map[string]any
	tenants []string
	names   []string // the actor's display name
}

func (f *fakeEvents) Event(ctx context.Context, tenantID, actor string, kind audit.Kind, subject string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events, f.data = append(f.events, string(kind)+" "+actor+" "+subject), append(f.data, fields)
	f.tenants, f.names = append(f.tenants, tenantID), append(f.names, audit.FromContext(ctx).ActorName)
}

func (f *fakeEvents) Refusal(ctx context.Context, tenantID, actor string, kind audit.Kind, outcome, subject string, fields map[string]any) {
	if outcome != audit.Refused {
		panic("a refusal's outcome: " + outcome)
	}
	f.Event(ctx, tenantID, actor, kind, subject, fields)
}

type fakeCounter struct {
	mu     sync.Mutex
	counts map[store.DownloadKey]int64
}

func (f *fakeCounter) Add(k store.DownloadKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[k]++
}

func (f *fakeCounter) total(authenticated bool) (n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, c := range f.counts {
		if k.Authenticated == authenticated {
			n += c
		}
	}
	return n
}

// Spec 0010 phase 2: what counts as a download, one install event per (principal, release,
// client, day), and the DuckDB routes' refusals.
func TestDownloadsAndInstalls(t *testing.T) {
	en, idp, adm := authEnv(t, "https")
	evs, cnt := &fakeEvents{}, &fakeCounter{counts: map[store.DownloadKey]int64{}}
	en.h.o.Events, en.h.o.Downloads = evs, cnt
	en.add(t, ext(t, 2000, 1, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "tresor"})
	priv := en.add(t, ext(t, 2000, 2, cpp("1.1")), release.AddOptions{Unchecked: true, Name: "tresor", Private: true})
	const (
		pubPath  = "/acme/prod/tresor/1.0/v2.0.0/linux_amd64/tresor.duckdb_extension.gz"
		privPath = "/acme/prod/tresor/1.1/v2.0.0/linux_amd64/tresor.duckdb_extension"
	)
	tok := idp.token(t, map[string]any{"name": "Alice A."})
	a := en.do(t, "GET", pubPath, nil)
	if a.status != 200 || cnt.total(false) != 1 {
		t.Fatalf("an anonymous download: %d %d", a.status, cnt.total(false))
	}
	for name, c := range map[string]struct {
		hdr  map[string]string
		want int
	}{
		"304":         {map[string]string{"If-None-Match": a.header.Get("ETag")}, 304},
		"later range": {map[string]string{"Range": "bytes=10-"}, 206},
	} {
		if r := en.do(t, "GET", pubPath, c.hdr); r.status != c.want {
			t.Fatalf("%s: %d", name, r.status)
		}
	}
	en.do(t, "HEAD", pubPath, nil)
	if n := cnt.total(false); n != 1 {
		t.Fatalf("HEAD, 304 and a later range count: %d", n)
	}
	if r := en.do(t, "GET", pubPath, map[string]string{"Range": "bytes=0-99"}); r.status != 206 || cnt.total(false) != 2 {
		t.Fatalf("a range from byte 0: %d %d", r.status, cnt.total(false))
	}
	// a valid token without install: refused; then granted, two downloads, one install event
	if r := en.do(t, "GET", privPath, bearerHdr(tok)); r.status != 404 {
		t.Fatalf("without a grant: %d", r.status)
	}
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, "", "tresor"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if r := en.do(t, "GET", privPath, bearerHdr(tok)); r.status != 200 {
			t.Fatalf("granted: %d", r.status)
		}
	}
	if n := cnt.total(true); n != 2 {
		t.Fatalf("authenticated downloads: %d", n)
	}
	expired := idp.token(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix(), "iat": time.Now().Add(-2 * time.Hour).Unix()})
	en.do(t, "GET", pubPath, bearerHdr(expired))
	evs.mu.Lock()
	defer evs.mu.Unlock()
	want := []string{
		"authz.refused principal:acme/",
		"install principal:acme/",
		"auth.failure anonymous route:extension",
	}
	if len(evs.events) != len(want) {
		t.Fatalf("events: %q", evs.events)
	}
	for i, w := range want {
		if len(evs.events[i]) < len(w) || evs.events[i][:len(w)] != w {
			t.Fatalf("event %d: %q, want %q…", i, evs.events[i], w)
		}
	}
	acme, _ := en.st.GetTenant(ctx, "acme")
	for i, tn := range evs.tenants {
		if tn != acme.ID {
			t.Fatalf("event %d's tenant: %s", i, tn)
		}
	}
	if evs.names[0] != "Alice A." || evs.names[1] != "Alice A." {
		t.Fatalf("display names: %q", evs.names)
	}
	if d := evs.data[0]; d["status"] != 404 || d["route"] != "GET extension" {
		t.Fatalf("the refusal's data: %v", d)
	}
	if d := evs.data[2]; d["reason"] != "invalid" || d["route"] != "GET extension" {
		t.Fatalf("the failure's data: %v", d)
	}
	d := evs.data[1]
	if d["release"] != priv.ID || d["version"] != "1.1" || d["duckdb_version"] != "v2.0.0" || d["user_agent"] == nil {
		t.Fatalf("the install's data: %v", d)
	}
	if _, err := audit.Data("install", d); err != nil {
		t.Fatalf("the install's fields are the catalogue's: %v", err)
	}
}
