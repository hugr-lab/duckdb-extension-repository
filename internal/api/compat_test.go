package api_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0046 in the management API's records: a feed says what waits, apart for what this kista
// cannot read and what arrived without content because the exporting kista could not (also in the
// answer of a change, not only of a read); an upstream says what its last run met of a source newer
// than this kista, as a member of its own, not as a text to match.
func TestOlderThanSourceRecords(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	tom := m.toks["tenant admin"]
	feed := `{"name":"air","url":"https://source.example/api/v1/tenants/x","mode":"bundle","keys":["sha256:` + strings.Repeat("a", 64) + `"]}`
	if r := m.call(t, "POST", T+"/feeds", tom, feed); r.status != 201 || r.json(t)["waiting"] != nil {
		t.Fatalf("add a feed: %d %s", r.status, r.body)
	}
	tn, _ := m.st.GetTenant(ctx, "acme")
	var f store.Feed
	if err := m.st.InTx(ctx, store.FeedLock(tn.ID), func(tx *store.Tx) (err error) {
		if f, err = tx.GetFeed(ctx, tn.ID, "air"); err != nil {
			return err
		}
		for body, w := range map[string]store.FeedWait{
			strings.Repeat("a", 64): {From: 3, Count: 2, Why: attachment.WaitKind, Level: attachment.ReaderLevel},
			strings.Repeat("b", 64): {From: 5, Count: 1, Why: attachment.WaitNewer, Level: 0},
		} {
			if err := tx.SetFeedCursor(ctx, f.ID, body, 9); err != nil {
				return err
			}
			w.BodyHash = body
			if err := tx.AddFeedWait(ctx, f.ID, w); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check := func(what string, rec map[string]any) {
		t.Helper()
		own, _ := rec["waiting"].(map[string]any)
		src, _ := rec["waiting_without_content"].(map[string]any)
		if len(own) != 1 || own[attachment.WaitKind] != float64(2) || len(src) != 1 || src[attachment.WaitNewer] != float64(1) {
			t.Fatalf("%s: waiting %v, without content %v", what, rec["waiting"], rec["waiting_without_content"])
		}
	}
	got := m.call(t, "GET", T+"/feeds/air", tom, "")
	check("the feed", got.json(t))
	check("the list", m.call(t, "GET", T+"/feeds", tom, "").json(t)["feeds"].([]any)[0].(map[string]any))
	r := m.call(t, "POST", T+"/feeds/air", tom, `{"readers":true}`, "If-Match", got.header.Get("ETag"))
	if r.status != 200 {
		t.Fatalf("a change: %d %s", r.status, r.body)
	}
	check("the answer of a change", r.json(t))

	// an upstream
	const U = T + "/upstreams"
	if r := m.call(t, "POST", U, tom, `{"name":"core","kind":"duckdb-core","channel":"staging","platforms":["linux_amd64"],"extensions":[{"name":"json"}]}`); r.status != 201 ||
		r.json(t)["older_than_source"] != nil {
		t.Fatalf("add an upstream: %d %s", r.status, r.body)
	}
	finish := func(result string) map[string]any {
		t.Helper()
		if err := m.st.InTx(ctx, "", func(tx *store.Tx) error {
			u, err := tx.GetUpstream(ctx, tn.ID, "core")
			if err != nil {
				return err
			}
			return tx.FinishUpstreamRun(ctx, u.ID, result, false, time.Now(), time.Time{})
		}); err != nil {
			t.Fatal(err)
		}
		return m.call(t, "GET", U+"/core", tom, "").json(t)
	}
	if rec := finish(`{"started":"2026-11-17T00:00:00Z","ended":"2026-11-17T00:00:01Z","counts":{"released":1}}`); rec["older_than_source"] != nil {
		t.Fatalf("a plain run: %v", rec["older_than_source"])
	}
	rec := finish(`{"started":"2026-11-17T00:00:00Z","ended":"2026-11-17T00:00:01Z","counts":{"revocations_unknown":2}}`)
	if a, _ := rec["older_than_source"].(map[string]any); a["revocations_unknown"] != float64(2) || a["api"] != nil {
		t.Fatalf("revocations in an unknown state: %v", rec["older_than_source"])
	}
	// the flag is the run's own member: an error that merely says the same words sets nothing
	rec = finish(`{"started":"2026-11-17T00:00:00Z","ended":"2026-11-17T00:00:01Z","counts":{},"error":"an api newer than this kista reads"}`)
	if rec["older_than_source"] != nil {
		t.Fatalf("an error's text read as the flag: %v", rec["older_than_source"])
	}
	rec = finish(`{"started":"2026-11-17T00:00:00Z","ended":"2026-11-17T00:00:01Z","counts":{},"error":"the run failed","newer_api":true}`)
	if a, _ := rec["older_than_source"].(map[string]any); a["api"] != true {
		t.Fatalf("a newer api: %v", rec["older_than_source"])
	}
}
