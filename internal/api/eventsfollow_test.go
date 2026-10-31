package api_test

import (
	"encoding/base64"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0040: following the events: a kind list or the kinds left out; with order=asc a settled
// tail, next always (the covered time on a short page, whatever the filter matched), and oldest_at.
func TestEventsFollow(t *testing.T) {
	base := time.Now().Add(time.Minute).Truncate(time.Second) // after the fixture's events
	now := base.Add(100 * time.Second)
	m := newMgmt(t, func(o *api.Options) { o.Now = func() time.Time { return now } }) // events.settle: the default 30s
	ta := m.toks["tenant admin"]
	const T = "/api/v1/tenants/acme"
	acme, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	stored := func(kind string, at time.Time) store.Event {
		t.Helper()
		e := store.Event{ID: store.NewID(), TenantID: acme.ID, At: at, Kind: kind, V: audit.Version, Outcome: audit.OK, Actor: "system:test",
			Subject: "x", Data: "{}", Request: store.NewID()}
		if err := m.st.InsertEvents(ctx, []store.Event{e}); err != nil {
			t.Fatal(err)
		}
		return e
	}
	read := func(q string) map[string]any {
		t.Helper()
		r := m.call(t, "GET", T+"/events?"+q, ta, "")
		if r.status != 200 {
			t.Fatalf("%s: %d %s", q, r.status, r.body)
		}
		return r.json(t)
	}
	ids := func(ans map[string]any) (out []string) {
		for _, e := range ans["events"].([]any) {
			out = append(out, e.(map[string]any)["id"].(string))
		}
		return out
	}
	covers := func(ans map[string]any, at time.Time) {
		t.Helper()
		next, _ := ans["next"].(string)
		b, _ := base64.RawURLEncoding.DecodeString(next)
		if string(b) != at.UTC().Format(time.RFC3339Nano)+"|" || ans["covered_at"] != at.UTC().Format(time.RFC3339Nano) {
			t.Errorf("next and covered_at are the covered time %v: %q %v", at, b, ans["covered_at"])
		}
	}
	e1 := stored("grant.add", base.Add(10*time.Second))
	e2 := stored("install", base.Add(20*time.Second))
	e3 := stored("grant.remove", base.Add(80*time.Second)) // newer than the window (now - 30s)
	since := "since=" + url.QueryEscape(base.Format(time.RFC3339)) + "&order=asc"

	// the kind list: kinds and families, or the kinds left out; refused: unknown, twice, mixed
	for q, want := range map[string][]string{
		"kind=grant.*,install":       {e1.ID, e2.ID},
		"kind=grant.add":             {e1.ID},
		"kind=!install":              {e1.ID},
		"kind=!grant.*,!tenant.read": {e2.ID},
		"kind=!install,!grant.add":   {},
	} {
		if got := ids(read(q + "&" + since)); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", q, got, want)
		}
	}
	for _, q := range []string{"kind=nope", "kind=nope.*", "kind=grant.add,grant.add", "kind=grant.add,!install", "kind=!grant.*,tenant.read",
		"kind=!install,!install", "kind=!", "kind=grant.add,", "kind=*", "kind=grant.add&kind=install"} {
		if r := m.call(t, "GET", T+"/events?"+q, ta, ""); r.status != 400 {
			t.Errorf("%s: %d %s", q, r.status, r.body)
		}
	}

	// a full page's next is its last event; a short page's the covered time, with or without a kind
	first := read(since + "&limit=1")
	if got := ids(first); !slices.Equal(got, []string{e1.ID}) || first["next"] == nil || first["covered_at"] != e1.At.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("a first page: %v", first)
	}
	tail := read(since + "&limit=5&cursor=" + url.QueryEscape(first["next"].(string)))
	if got := ids(tail); !slices.Equal(got, []string{e2.ID}) {
		t.Fatalf("the tail (the newest event not yet): %v", tail)
	}
	covers(tail, now.Add(-30*time.Second))
	filtered := read(since + "&kind=grant.remove")
	if len(ids(filtered)) != 0 {
		t.Fatalf("a filtered tail: %v", filtered)
	}
	covers(filtered, now.Add(-30*time.Second))
	covers(read(since+"&until="+url.QueryEscape(base.Add(50*time.Second).Format(time.RFC3339))), base.Add(50*time.Second))
	if d := read("order=desc&limit=500"); d["next"] != nil || d["covered_at"] != nil {
		t.Errorf("order=desc on a short page: %v %v", d["next"], d["covered_at"])
	}
	// a cursor past now - settle (another replica's clock ahead, or a smaller window) comes back
	// unchanged: next never moves back
	now = now.Add(-10 * time.Second)
	if back := read(since + "&cursor=" + url.QueryEscape(tail["next"].(string))); back["next"] != tail["next"] ||
		back["covered_at"] != tail["covered_at"] || len(ids(back)) != 0 {
		t.Errorf("a cursor past the covered time: %v, want %v", back, tail["next"])
	}
	now = now.Add(10 * time.Second)

	// an event stored late (its at 10s before now, after a read at the tail) is answered by the next
	// read from that read's next, once it settles
	late := stored("grant.add", now.Add(-10*time.Second))
	cur := url.QueryEscape(tail["next"].(string))
	if got := ids(read(since + "&cursor=" + cur)); len(got) != 0 {
		t.Fatalf("events newer than the window: %v", got)
	}
	now = now.Add(time.Minute)
	if got := ids(read(since + "&cursor=" + cur)); !slices.Equal(got, []string{e3.ID, late.ID}) {
		t.Fatalf("after the window: %v", got)
	}

	// with no cursor the read starts at the oldest retained event; oldest_at follows retention
	all := read("order=asc&limit=1")
	oldest, _ := all["oldest_at"].(string)
	if oldest == "" || all["events"].([]any)[0].(map[string]any)["at"] != oldest {
		t.Errorf("the first event is the oldest: %v", all)
	}
	if _, _, err := m.st.PruneEvents(ctx, base.Add(15*time.Second), base.Add(15*time.Second), 100000); err != nil {
		t.Fatal(err)
	}
	if got := read("order=asc")["oldest_at"]; got != e2.At.UTC().Format(time.RFC3339Nano) {
		t.Errorf("oldest_at after pruning: %v, want %v", got, e2.At)
	}
}
