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
		if n, m, _ := s.PruneEvents(ctx, now.Add(-24*time.Hour), now.Add(-72*time.Hour), 100); n != 0 || m != 0 {
			t.Fatalf("an undelivered event pruned before it was overdue: %d", n)
		}
		if _, m, _ := s.PruneEvents(ctx, now.Add(-24*time.Hour), now.Add(-24*time.Hour), 100); m != 1 {
			t.Fatalf("an overdue event kept: %d", m)
		}
		if err := store.ExecRaw(ctx, s, "DELETE FROM events WHERE kind = 'authz.refused'"); err != nil {
			t.Fatal(err)
		}
		// pruning: delivered events before the cutoff, then a tenant's overflow
		if n, _, err := s.PruneEvents(ctx, got[1].At, now.Add(-time.Hour), 100); err != nil || n != 1 {
			t.Fatalf("prune: %d %v", n, err)
		}
		if n, _, err := s.PruneTenantOverflow(ctx, 1, 100); err != nil || n != 1 {
			t.Fatalf("overflow: %d %v", n, err)
		}
		if r, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{}); len(r) != 1 || r[0].Kind != "channel.create" {
			t.Fatalf("after pruning: %+v", r)
		}
	})
}

func TestEventSinks(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return now }
		reg, err := s.RegisterSinks(ctx, []string{"a", "b"})
		if err != nil || len(reg) != 2 || reg[0].Bit != 0 || reg[1].Bit != 1 {
			t.Fatalf("register: %+v %v", reg, err)
		}
		// a sink listed since is not retired; one listed before is, its bit reserved until dropped
		now = now.Add(time.Minute)
		if ok, err := s.RetireSink(ctx, "a", now.Add(-2*time.Minute)); err != nil || ok {
			t.Fatalf("retired a listed sink: %v %v", ok, err)
		}
		if ok, err := s.RetireSink(ctx, "a", now); err != nil || !ok {
			t.Fatalf("retire: %v %v", ok, err)
		}
		reg, err = s.RegisterSinks(ctx, []string{"b", "a"})
		if err != nil || len(reg) != 3 || !reg[0].Removing || reg[0].Bit != 0 || reg[2].Name != "a" || reg[2].Bit != 2 {
			t.Fatalf("a re-added sink gets a new bit while the tombstone holds the old: %+v %v", reg, err)
		}
		if err := s.DropSink(ctx, "a"); err == nil {
			t.Fatal("dropped a live sink")
		}
		if err := s.DropSink(ctx, reg[0].Name); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.RetireSink(ctx, "a", now.Add(time.Second)); err != nil || !ok {
			t.Fatalf("retire a again: %v %v", ok, err)
		}
		if err := s.DropSink(ctx, "~removing:2"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		reg, err = s.RegisterSinks(ctx, []string{"b", "c"})
		if err != nil || len(reg) != 2 {
			t.Fatalf("re-register: %+v %v", reg, err)
		}
		bits := map[string]int{}
		for _, k := range reg {
			bits[k.Name] = k.Bit
			if !k.LastListedAt.Equal(now) {
				t.Fatalf("listed at: %+v", k)
			}
		}
		if bits["b"] != 1 || bits["c"] != 0 {
			t.Fatalf("bits: %v", bits)
		}
		s.EventSinks = func(string) int { return 1<<0 | 1<<1 }
		var ids []string
		for i := range 3 {
			now = now.Add(time.Second)
			ev, err := s.NewEvent(ctx, tn.ID, "os:1:t", "grant.add", audit.OK, "grant:"+string(rune('a'+i)), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.InsertEvents(ctx, []store.Event{ev}); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, ev.ID)
		}
		got, err := s.PendingEvents(ctx, 1<<1, 2)
		if err != nil || len(got) != 2 || got[0].ID != ids[0] || got[1].ID != ids[1] {
			t.Fatalf("pending in order: %+v %v", got, err)
		}
		if err := s.ClearPending(ctx, 1<<1, ids[:2]); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.PendingEvents(ctx, 1<<1, 10); len(got) != 1 || got[0].ID != ids[2] {
			t.Fatalf("after clearing b: %+v", got)
		}
		if got, _ := s.PendingEvents(ctx, 1<<0, 10); len(got) != 3 || got[0].Pending != 1 {
			t.Fatalf("c keeps its bit: %+v", got)
		}
		if n, err := s.ClearSinkBit(ctx, store.TombstoneName(0), 1<<0, 2); err != nil || n != 0 {
			t.Fatalf("no tombstone, nothing cleared: %d %v", n, err)
		}
		if ok, err := s.RetireSink(ctx, "c", now.Add(time.Second)); err != nil || !ok {
			t.Fatalf("retire c: %v %v", ok, err)
		}
		n, err := s.ClearSinkBit(ctx, store.TombstoneName(0), 1<<0, 2)
		if err != nil || n != 2 {
			t.Fatalf("clear a sink's bit: %d %v", n, err)
		}
		if n, _ := s.ClearSinkBit(ctx, store.TombstoneName(0), 1<<0, 2); n != 1 {
			t.Fatalf("the rest: %d", n)
		}
		if got, _ := s.PendingEvents(ctx, 1<<0, 10); len(got) != 0 {
			t.Fatalf("cleared: %+v", got)
		}
		// delivered events are pruned at the retention; undelivered ones only when overdue
		_, _, _ = s.PruneEvents(ctx, now.Add(time.Hour), now.Add(-time.Hour), 10)
		if got, _ := s.ListEvents(ctx, tn.ID, store.EventFilter{}); len(got) != 1 || got[0].ID != ids[2] {
			t.Fatalf("undelivered kept: %+v", got)
		}
		if _, err := s.LastEvent(ctx, tn.ID, "grant.remove"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		if ev, err := s.LastEvent(ctx, tn.ID, "grant.add"); err != nil || ev.ID != ids[2] {
			t.Fatalf("last: %+v %v", ev, err)
		}
	})
}

func TestEventSinkBitsRunOut(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		var names []string
		for i := range store.MaxSinks + 1 {
			names = append(names, "s"+string(rune('a'+i)))
		}
		reg, err := s.RegisterSinks(ctx, names)
		if !errors.Is(err, store.ErrNoSinkBit) || len(reg) != store.MaxSinks {
			t.Fatalf("17 sinks: %d %v", len(reg), err)
		}
	})
}
