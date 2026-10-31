package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0040: -follow pages through more events than a page holds in one window, prints each once,
// oldest first, none newer than now - settle, and a late one once it settles.
func TestFollowEvents(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const tenant = "t1"
	event := func(at time.Time) store.Event {
		return store.Event{ID: store.NewID(), TenantID: tenant, At: at, Kind: "grant.add", V: audit.Version, Outcome: audit.OK,
			Actor: "system:test", Subject: "x", Data: "{}", Request: store.NewID()}
	}
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var many []store.Event
	for i := range 2500 { // more than a page, within one window
		many = append(many, event(base.Add(time.Duration(i)*time.Millisecond)))
	}
	newer := event(base.Add(20 * time.Second)) // newer than the window at first
	if err := st.InsertEvents(ctx, append(many, newer)); err != nil {
		t.Fatal(err)
	}
	now := base.Add(40 * time.Second) // settle 30s: the first reads stop at base+10s
	var late store.Event
	pauses := 0
	pause := func(context.Context) bool {
		pauses++
		if pauses == 1 { // an event committed late, before the newer one settles
			late = event(base.Add(15 * time.Second))
			if err := st.InsertEvents(ctx, []store.Event{late}); err != nil {
				t.Fatal(err)
			}
			now = now.Add(30 * time.Second)
			return true
		}
		return false
	}
	var got []store.Event
	f := store.EventFilter{Since: base.Add(-time.Minute), Limit: 1000}
	if err := followEvents(ctx, st, tenant, f, 30*time.Second, func() time.Time { return now }, pause,
		func(evs []store.Event) { got = append(got, evs...) }); err != nil {
		t.Fatal(err)
	}
	want := append(append(many, late), newer)
	if len(got) != len(want) {
		t.Fatalf("printed %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("event %d: %s at %v, want %s at %v", i, got[i].ID, got[i].At, want[i].ID, want[i].At)
		}
	}
}
