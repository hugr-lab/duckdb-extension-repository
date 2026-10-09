package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

func TestEvents(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return now }
		rctx := audit.WithRequest(ctx, audit.Request{Client: "203.0.113.77", ID: "req-1", ActorName: "Alice"})
		// an event rolls back with its change
		boom := errors.New("boom")
		if err := s.InTx(rctx, "", func(tx *store.Tx) error {
			if err := tx.Event(rctx, tn.ID, "os:1:t", "grant.add", "grant:1", map[string]any{"principal": "x"}); err != nil {
				return err
			}
			return boom
		}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		// an unknown field never reaches an event
		if err := s.InTx(rctx, "", func(tx *store.Tx) error {
			return tx.Event(rctx, tn.ID, "os:1:t", "grant.add", "grant:1", map[string]any{"token": "secret"})
		}); err == nil {
			t.Fatal("a field outside the catalogue")
		}
		for i, k := range []audit.Kind{"grant.add", "grant.remove", "channel.create"} {
			now = now.Add(time.Minute)
			subj := "grant:" + string(rune('a'+i))
			if k == "channel.create" {
				subj = "channel:prod_x%"
			}
			inTx(t, s, func(tx *store.Tx) error {
				return tx.Event(rctx, tn.ID, "os:1:t", k, subj, map[string]any{})
			})
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.Event(ctx, "", "os:1:t", "version.add", "version:v2", nil) })
		got, err := s.ListEvents(ctx, tn.ID, store.EventFilter{})
		if err != nil || len(got) != 3 || got[0].Kind != "channel.create" || got[2].Kind != "grant.add" {
			t.Fatalf("newest first: %+v %v", got, err)
		}
		if g := got[2]; g.Client != "203.0.113.0/24" || g.Request != "req-1" || g.ActorName != "Alice" || g.V != 1 || g.Outcome != "ok" {
			t.Fatalf("the request's facts: %+v", g)
		}
		page, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{Limit: 1, Ascending: true})
		next, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{Limit: 5, Ascending: true, AfterAt: page[0].At, AfterID: page[0].ID})
		if len(page) != 1 || len(next) != 2 || next[0].Kind != "grant.remove" {
			t.Fatalf("paging: %+v %+v", page, next)
		}
		for name, f := range map[string]store.EventFilter{
			"kind":    {Kind: "grant.remove"},
			"subject": {Subject: "channel:prod_x%"},
			"since":   {Since: got[0].At},
		} {
			if r, err := s.ListEvents(ctx, tn.ID, f); err != nil || len(r) != 1 {
				t.Errorf("%s: %+v %v", name, r, err)
			}
		}
		if r, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{Subject: "channel:prod_"}); len(r) != 1 {
			t.Errorf("a subject prefix: %+v", r)
		}
		if r, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{Subject: "CHANNEL:"}); len(r) != 0 {
			t.Errorf("a subject prefix ignores case: %+v", r)
		}
		if r, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{Subject: "grant:%"}); len(r) != 0 {
			t.Errorf("a LIKE metacharacter is literal: %+v", r)
		}
		srv, _ := s.ListEvents(ctx, audit.ServerTenant, store.EventFilter{})
		if len(srv) != 1 || srv[0].Request != srv[0].ID || srv[0].Client != "" {
			t.Fatalf("server events: %+v", srv)
		}
		if ev, err := s.GetEvent(ctx, tn.ID, got[1].ID); err != nil || ev.Kind != "grant.remove" {
			t.Fatalf("get: %+v %v", ev, err)
		}
		if _, err := s.GetEvent(ctx, "other", got[1].ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another tenant's event: %v", err)
		}
		// actor and outcome filters, newest first with a cursor
		now = now.Add(time.Minute)
		ref, err := s.NewEvent(rctx, tn.ID, "principal:acme/i|sub:bob", "authz.refused", audit.Refused, "route:x", map[string]any{"status": 404})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.InsertEvents(ctx, []store.Event{ref}); err != nil {
			t.Fatal(err)
		}
		if r, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{Outcome: audit.Refused}); len(r) != 1 || r[0].Actor != ref.Actor {
			t.Errorf("by outcome: %+v", r)
		}
		if r, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{Actor: "os:1:t"}); len(r) != 3 {
			t.Errorf("by actor: %+v", r)
		}
		d1, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{Limit: 2})
		d2, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{Limit: 5, AfterAt: d1[1].At, AfterID: d1[1].ID})
		if len(d1) != 2 || d1[0].Kind != "authz.refused" || len(d2) != 2 || d2[1].Kind != "grant.add" {
			t.Fatalf("desc paging: %+v %+v", d1, d2)
		}
		// an undelivered event stays past the retention until it is overdue
		late := ref
		late.ID, late.At, late.Pending = store.NewID(), now.Add(-48*time.Hour), 1
		if err := s.InsertEvents(ctx, []store.Event{late}); err != nil {
			t.Fatal(err)
		}
		if n, _ := s.PruneEvents(ctx, now.Add(-24*time.Hour), now.Add(-72*time.Hour), 100); n != 0 {
			t.Fatalf("an undelivered event pruned before it was overdue: %d", n)
		}
		if n, _ := s.PruneEvents(ctx, now.Add(-24*time.Hour), now.Add(-24*time.Hour), 100); n != 1 {
			t.Fatalf("an overdue event kept: %d", n)
		}
		if err := store.ExecRaw(ctx, s, "DELETE FROM events WHERE kind = 'authz.refused'"); err != nil {
			t.Fatal(err)
		}
		// pruning: delivered events before the cutoff, then a tenant's overflow
		if n, err := s.PruneEvents(ctx, got[1].At, now.Add(-time.Hour), 100); err != nil || n != 1 {
			t.Fatalf("prune: %d %v", n, err)
		}
		if n, err := s.PruneTenantOverflow(ctx, 1, 100); err != nil || n != 1 {
			t.Fatalf("overflow: %d %v", n, err)
		}
		if r, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{}); len(r) != 1 || r[0].Kind != "channel.create" {
			t.Fatalf("after pruning: %+v", r)
		}
	})
}
